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
	"flag"
	"fmt"
	"net/url"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

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
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var prometheusURL string
	var runDoorman bool
	var doormanService, doormanNamespace string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics via HTTPS. Use --metrics-secure=false for HTTP.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "Directory containing the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "Webhook certificate file name.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "Webhook key file name.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "Metrics server certificate file name.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "Metrics server key file name.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "Enable HTTP/2 for metrics and webhook servers.")
	flag.BoolVar(&runDoorman, "doorman", false,
		"Run as the doorman, which holds connections to paused workloads and wakes them, instead of the operator.")
	flag.StringVar(&doormanService, "doorman-service", "hybernate-doorman",
		"Name of the doorman's Service. Empty disables waking on request.")
	flag.StringVar(&doormanNamespace, "doorman-namespace", envOr("POD_NAMESPACE", "hybernate-system"),
		"Namespace of the doorman's Service.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"Base URL of the Prometheus API used for activity queries, e.g. http://prometheus.monitoring.svc:9090.")
	optIn := controller.DefaultOptInDefaults
	flag.DurationVar(&optIn.IdleAfter, "default-idle-after", optIn.IdleAfter,
		"idleAfter for workloads opted in with the hybernate.io/managed label, unless annotated otherwise.")
	flag.IntVar(&optIn.CPUThreshold, "default-cpu-threshold", optIn.CPUThreshold,
		"CPU threshold, as a percentage of requests, for workloads opted in with the label, unless annotated otherwise.")
	flag.BoolVar(&optIn.DryRun, "default-dry-run", optIn.DryRun,
		"Measure workloads opted in with the label without pausing them, unless annotated otherwise.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := validatePrometheusURL(prometheusURL); err != nil {
		setupLog.Error(err, "invalid --prometheus-url")
		os.Exit(1)
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}

	webhookServerOptions := webhook.Options{TLSOpts: tlsOpts}
	if len(webhookCertPath) > 0 {
		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhook.NewServer(webhookServerOptions),
		HealthProbeBindAddress: probeAddr,
		// Every doorman replica serves traffic, so only the operator elects a leader.
		LeaderElection:   enableLeaderElection && !runDoorman,
		LeaderElectionID: "479a98fc.hybernate.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&metricsv1beta1.PodMetrics{}},
			},
		},
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
		server := doorman.NewServer(c, mgr.GetEventRecorder("hybernate-doorman"), "")
		if err := mgr.Add(server); err != nil {
			setupLog.Error(err, "unable to add doorman")
			os.Exit(1)
		}
		readyz = server.Ready
	} else {
		if err := (&controller.Reconciler{
			Client:           c,
			Scheme:           mgr.GetScheme(),
			Recorder:         mgr.GetEventRecorder("hybernate"),
			PrometheusURL:    prometheusURL,
			DoormanService:   doormanService,
			DoormanNamespace: doormanNamespace,
			PodReader:        mgr.GetAPIReader(),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ManagedWorkload")
			os.Exit(1)
		}
		for _, kind := range []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet} {
			if err := (&controller.OptInReconciler{
				Client:   c,
				Scheme:   mgr.GetScheme(),
				Recorder: mgr.GetEventRecorder("hybernate"),
				Kind:     kind,
				Defaults: optIn,
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
