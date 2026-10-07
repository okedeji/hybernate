/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	// The image is distroless, with no zoneinfo, so --timezone needs the
	// database compiled in.
	_ "time/tzdata"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/controller"
	"github.com/okedeji/hybernate/internal/doorman"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(metricsv1beta1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var prometheusURL string
	var runDoorman bool
	var doormanService, doormanNamespace, doormanHealthCheckAgents string
	var secureMetrics bool
	var enableHTTP2 bool
	var maxConcurrentReconciles int
	var timezone string
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics via HTTPS. Use --metrics-secure=false for HTTP.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "Metrics server certificate file name.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "Metrics server key file name.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "Enable HTTP/2 for the metrics server.")
	flag.BoolVar(&runDoorman, "doorman", false,
		"Run as the doorman, which holds connections to paused workloads and wakes them, instead of the operator.")
	flag.StringVar(&doormanService, "doorman-service", "hybernate-doorman",
		"Name of the doorman's Service. Empty disables waking on request.")
	flag.StringVar(&doormanNamespace, "doorman-namespace", envOr("POD_NAMESPACE", "hybernate-system"),
		"Namespace of the doorman's Service.")
	flag.StringVar(&doormanHealthCheckAgents, "doorman-health-check-user-agents", "",
		"Comma-separated User-Agent prefixes of health checkers and scrapers whose GET and HEAD requests don't wake "+
			"a paused workload, besides the built-in ones, such as MyCorpMonitor/.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"Base URL of the Prometheus API used for activity queries, e.g. http://prometheus.monitoring.svc:9090.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 4,
		"How many ManagedWorkloads are reconciled at once, so one slow metrics or Prometheus query doesn't hold up wakes.")
	flag.StringVar(&timezone, "timezone", "UTC",
		"IANA time zone, such as Europe/London, whose hours and weekdays forecasts learn, so they follow daylight saving.")
	optIn := controller.DefaultOptInDefaults
	flag.DurationVar(&optIn.IdleAfter, "default-idle-after", optIn.IdleAfter,
		"idleAfter for workloads opted in with the hybernate.io/managed label, unless annotated otherwise.")
	flag.IntVar(&optIn.CPUThreshold, "default-cpu-threshold", optIn.CPUThreshold,
		"CPU threshold, as a percentage of requests, for workloads opted in with the label, unless annotated otherwise.")
	flag.BoolVar(&optIn.DryRun, "default-dry-run", optIn.DryRun,
		"Measure workloads opted in with the label without pausing them, unless annotated otherwise.")
	var watched stringList
	flag.Var(&watched, "watch-namespaces",
		"Comma-separated namespaces to work in, with a Role in each. Empty means every namespace.")
	var protected stringList
	flag.Var(&protected, "protected-namespaces",
		"Comma-separated name patterns, such as prod-*, of namespaces Hybernate never manages, as if labelled "+
			v1alpha1.LabelProtected+", unless labelled "+v1alpha1.LabelAllowProtected+".")

	var opts zap.Options
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if limit, ok := softMemoryLimit(os.Getenv("GOMEMLIMIT"), os.Getenv(envMemoryLimit)); ok {
		debug.SetMemoryLimit(limit)
	}
	if err := validatePatterns(protected); err != nil {
		setupLog.Error(err, "invalid --protected-namespaces")
		os.Exit(1)
	}
	if err := validatePrometheusURL(prometheusURL); err != nil {
		setupLog.Error(err, "invalid --prometheus-url")
		os.Exit(1)
	}
	if err := validateOptInDefaults(optIn); err != nil {
		setupLog.Error(err, "invalid --default-* flag")
		os.Exit(1)
	}
	if maxConcurrentReconciles < 1 {
		setupLog.Error(errors.New("must be at least 1"), "invalid --max-concurrent-reconciles",
			"value", maxConcurrentReconciles)
		os.Exit(1)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		setupLog.Error(err, "invalid --timezone")
		os.Exit(1)
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if len(metricsCertPath) > 0 {
		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	cacheOpts := doormanCacheOptions(watched)
	if !runDoorman {
		routedNamespace := doormanNamespace
		if doormanService == "" {
			routedNamespace = ""
		}
		cacheOpts = operatorCacheOptions(watched, routedNamespace)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		// Every doorman replica serves traffic, so only the operator elects a leader.
		LeaderElection:   enableLeaderElection && !runDoorman,
		LeaderElectionID: "479a98fc.hybernate.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				// ConfigMaps are read one at a time, for the few a workload
				// references; caching them would hold every one in the cluster.
				DisableFor: []client.Object{&metricsv1beta1.PodMetrics{}, &corev1.ConfigMap{}},
			},
		},
		Cache: cacheOpts,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Writes are made under Hybernate's own field manager, so the replicas
	// it pauses are attributed to it rather than to the binary's name.
	c := client.WithFieldOwner(mgr.GetClient(), v1alpha1.FieldManager)
	readyz := healthz.Ping
	if runDoorman {
		server := doorman.NewServer(c, mgr.GetEventRecorder("hybernate-doorman"), doorman.Options{
			Informers:         mgr.GetCache(),
			HealthCheckAgents: strings.Split(doormanHealthCheckAgents, ","),
		})
		if err := mgr.Add(server); err != nil {
			setupLog.Error(err, "unable to add doorman")
			os.Exit(1)
		}
		readyz = server.Ready
	} else {
		if err := (&controller.Reconciler{
			Client:                  c,
			Scheme:                  mgr.GetScheme(),
			Recorder:                mgr.GetEventRecorder("hybernate"),
			PrometheusURL:           prometheusURL,
			DoormanService:          doormanService,
			DoormanNamespace:        doormanNamespace,
			PodReader:               mgr.GetAPIReader(),
			ProtectedNamespaces:     protected,
			WatchNamespaces:         watched,
			MaxConcurrentReconciles: maxConcurrentReconciles,
			Timezone:                location,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ManagedWorkload")
			os.Exit(1)
		}
		for _, kind := range []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet} {
			if err := (&controller.OptInReconciler{
				Client:              c,
				Scheme:              mgr.GetScheme(),
				Recorder:            mgr.GetEventRecorder("hybernate"),
				Kind:                kind,
				Defaults:            optIn,
				ProtectedNamespaces: protected,
				WatchNamespaces:     watched,
			}).SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "OptIn", "kind", kind)
				os.Exit(1)
			}
		}
		// +kubebuilder:scaffold:builder
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// envMemoryLimit is the container's memory limit in bytes, which the
// manifests set from the pod's resources.
const envMemoryLimit = "MEMORY_LIMIT"

// softMemoryLimit is the limit for Go's garbage collector: 90% of the
// container's, leaving the rest for what the runtime holds outside the
// heap, such as goroutine stacks and the memory it has yet to return.
// GOMEMLIMIT, when set, is left for the runtime to apply as given.
func softMemoryLimit(gomemlimit, containerLimit string) (int64, bool) {
	if gomemlimit != "" {
		return 0, false
	}
	limit, err := strconv.ParseInt(containerLimit, 10, 64)
	if err != nil || limit <= 0 {
		return 0, false
	}
	return limit / 10 * 9, true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// validatePrometheusURL fails fast on a malformed URL so a typo surfaces at
// startup instead of as a signal error on every reconcile. Empty is valid:
// Prometheus signals are optional.
func validatePrometheusURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// validateOptInDefaults applies the rules a workload's own annotations are
// held to. A default outside them would be written into every label-created
// ManagedWorkload and fail its validation, or, for a CPU threshold of 0,
// silently become the CRD's default of 10.
func validateOptInDefaults(d controller.OptInDefaults) error {
	if d.IdleAfter <= 0 {
		return fmt.Errorf("--default-idle-after %s must be positive", d.IdleAfter)
	}
	if d.CPUThreshold < 1 || d.CPUThreshold > 100 {
		return fmt.Errorf("--default-cpu-threshold %d must be from 1 to 100", d.CPUThreshold)
	}
	return nil
}

// cacheOptions limits the cache to the watched namespaces, or leaves
// it cluster-wide when there are none. Cluster-scoped objects, such as
// nodes, are cached either way.
func cacheOptions(watched []string) cache.Options {
	return cache.Options{
		DefaultNamespaces: namespaceCaches(watched),
		DefaultTransform:  trimForCache,
	}
}

// operatorCacheOptions adds to cacheOptions the operator's view of
// EndpointSlices (see controller.EndpointSliceCache). Empty
// doormanNamespace means the doorman is disabled.
func operatorCacheOptions(watched []string, doormanNamespace string) cache.Options {
	opts := cacheOptions(watched)
	opts.ByObject = map[client.Object]cache.ByObject{
		&discoveryv1.EndpointSlice{}: controller.EndpointSliceCache(watched, doormanNamespace),
	}
	return opts
}

// doormanCacheOptions adds to cacheOptions the doorman's view of
// EndpointSlices (see doorman.EndpointSliceCache).
func doormanCacheOptions(watched []string) cache.Options {
	opts := cacheOptions(watched)
	opts.ByObject = map[client.Object]cache.ByObject{
		&discoveryv1.EndpointSlice{}: doorman.EndpointSliceCache(),
	}
	return opts
}

// namespaceCaches limits the cache to the watched namespaces, or leaves it
// cluster-wide when there are none.
func namespaceCaches(namespaces []string) map[string]cache.Config {
	if len(namespaces) == 0 {
		return nil
	}
	out := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		out[ns] = cache.Config{}
	}
	return out
}

// replicasOnly is all of a managed fields entry that gitops.ReplicasWriter
// reads: that it set spec.replicas.
var replicasOnly = &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)}

// trimForCache drops what the operator never reads from cached objects,
// which on a large cluster is most of their size: managed fields, and
// kubectl's copy of the last applied manifest. Deployments and StatefulSets
// keep the managed fields entries that set spec.replicas, cut down to that,
// which is how a GitOps tool undoing a pause is told apart from a person.
// ManagedWorkloads keep the last applied manifest, since the operator
// updates them whole and would otherwise delete it.
func trimForCache(in any) (any, error) {
	obj, err := meta.Accessor(in)
	if err != nil {
		return in, nil
	}
	switch in.(type) {
	case *appsv1.Deployment, *appsv1.StatefulSet:
		obj.SetManagedFields(replicasWriters(obj.GetManagedFields()))
	default:
		obj.SetManagedFields(nil)
	}
	if _, ok := in.(*v1alpha1.ManagedWorkload); !ok {
		if annotations := obj.GetAnnotations(); annotations != nil {
			delete(annotations, corev1.LastAppliedConfigAnnotation)
		}
	}
	return in, nil
}

func replicasWriters(entries []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
	var out []metav1.ManagedFieldsEntry
	for _, entry := range entries {
		if entry.FieldsV1 == nil || !setsSpecReplicas(entry.FieldsV1.Raw) {
			continue
		}
		out = append(out, metav1.ManagedFieldsEntry{
			Manager:   entry.Manager,
			Operation: entry.Operation,
			Time:      entry.Time,
			FieldsV1:  replicasOnly,
		})
	}
	return out
}

func setsSpecReplicas(raw []byte) bool {
	var fields struct {
		Spec map[string]json.RawMessage `json:"f:spec"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, ok := fields.Spec["f:replicas"]
	return ok
}

// stringList is a flag of comma-separated values.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	for v := range strings.SplitSeq(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*l = append(*l, v)
		}
	}
	return nil
}

// validatePatterns rejects a namespace pattern that isn't a valid glob,
// which would otherwise match nothing and protect nothing.
func validatePatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
	}
	return nil
}
