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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// version is the plugin's release, set at build time with
// -ldflags "-X main.version=$TAG".
var version = "dev"

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = metricsv1beta1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
}

func main() {
	if err := newRootCmd(newKubeClient).Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd(newClient clientFunc) *cobra.Command {
	root := &cobra.Command{
		Use:   "kubectl-hybernate",
		Short: "Hybernate kubectl plugin for workload lifecycle management",
	}
	kube := addKubeFlags(root.PersistentFlags(), newClient)
	root.AddCommand(depsCmd(kube), enableCmd(kube), scanCmd(kube), statusCmd(kube), versionCmd(), wakeCmd(kube))
	for _, cmd := range root.Commands() {
		silenceUsageOnceRunning(cmd)
	}
	return root
}

// silenceUsageOnceRunning stops cobra printing usage for an error from the
// command itself. Usage helps with an unknown flag or a wrong number of
// arguments, which cobra reports before RunE starts, but not with an error
// from the cluster.
func silenceUsageOnceRunning(cmd *cobra.Command) {
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return run(cmd, args)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin's version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "kubectl-hybernate %s\n", version)
			return err
		},
	}
}

// clientFunc makes the client every command talks to the cluster through.
type clientFunc func(*rest.Config) (client.Client, error)

func newKubeClient(config *rest.Config) (client.Client, error) {
	return client.New(config, client.Options{Scheme: scheme})
}

// A CLI makes its calls in bursts and then exits, so it can go well past
// client-go's default of 5 requests a second, which would make a scan of a
// few hundred workloads take minutes.
const (
	clientQPS   = 50
	clientBurst = 100
)

// apiRequestTimeout bounds each request to the API server unless
// --request-timeout says otherwise. The commands make many small requests,
// so one that takes this long means the server isn't answering.
const apiRequestTimeout = 30 * time.Second

// kubeFlags are kubectl's connection flags: --kubeconfig, --context,
// --cluster, --user, --as and the rest. They're bound with clientcmd rather
// than k8s.io/cli-runtime's ConfigFlags, which would pull kustomize and the
// resource builder into the binary for the same flags.
type kubeFlags struct {
	rules     *clientcmd.ClientConfigLoadingRules
	overrides clientcmd.ConfigOverrides
	newClient clientFunc
}

func addKubeFlags(flags *pflag.FlagSet, newClient clientFunc) *kubeFlags {
	k := &kubeFlags{rules: clientcmd.NewDefaultClientConfigLoadingRules(), newClient: newClient}
	flags.StringVar(&k.rules.ExplicitPath, clientcmd.RecommendedConfigPathFlag, "",
		"Path to the kubeconfig file to use")
	names := clientcmd.RecommendedConfigOverrideFlags("")
	// Each command defines its own -n: status and scan take several.
	names.ContextOverrideFlags.Namespace = clientcmd.FlagInfo{}
	names.Timeout.Default = apiRequestTimeout.String()
	names.Timeout.Description = "How long to wait for each request to the API server before giving up, such as " +
		"10s or 2m; 0 means the default"
	clientcmd.BindOverrideFlags(&k.overrides, flags, names)
	return k
}

// cluster is the kubeconfig context a command talks to, and that context's
// namespace, which is what kubectl uses when -n isn't given.
type cluster struct {
	context   string
	namespace string
}

// restConfig loads the kubeconfig the flags select. Every request made with
// it ends when ctx does.
func (k *kubeFlags) restConfig(ctx context.Context) (*rest.Config, cluster, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(k.rules, &k.overrides)
	at := cluster{context: k.overrides.CurrentContext}
	if raw, err := loader.RawConfig(); err == nil && at.context == "" {
		at.context = raw.CurrentContext
	}
	config, err := loader.ClientConfig()
	if err != nil {
		return nil, at, fmt.Errorf("loading kubeconfig: %w", err)
	}
	at.namespace, _, err = loader.Namespace()
	if err != nil {
		return nil, at, fmt.Errorf("reading kubeconfig namespace: %w", err)
	}
	config.QPS, config.Burst = clientQPS, clientBurst
	if config.Timeout == 0 {
		config.Timeout = apiRequestTimeout
	}
	config.Wrap(cancelWith(ctx))
	return config, at, nil
}

func (k *kubeFlags) client(ctx context.Context) (client.Client, cluster, error) {
	config, at, err := k.restConfig(ctx)
	if err != nil {
		return nil, at, err
	}
	c, err := k.newClient(config)
	if err != nil {
		return nil, at, fmt.Errorf("creating client: %w", err)
	}
	return c, at, nil
}

// cancelWith ends every request to the API server when ctx ends. The
// client's API discovery doesn't take a context, so without it a command
// past its deadline still waits out each discovery request in turn.
func cancelWith(ctx context.Context) func(http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			reqCtx, cancel := context.WithCancelCause(r.Context())
			context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
			return rt.RoundTrip(r.WithContext(reqCtx))
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func addTimeoutFlag(cmd *cobra.Command, timeout *time.Duration, usage string) {
	cmd.Flags().DurationVar(timeout, "timeout", *timeout, usage)
}

// checkPositive rejects a duration flag of zero or less, which would end a
// command before it starts or look back to the future.
func checkPositive(flag string, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("--%s must be more than zero", flag)
	}
	return nil
}

// timedOut says a command ran out of --timeout, instead of the bare
// "context deadline exceeded" from whichever call it was in.
func timedOut(ctx context.Context, err error, timeout time.Duration) error {
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("the cluster's API server didn't answer within --timeout %s; pass a longer --timeout: %w",
		timeout, err)
}

// namespaceFlags are -n, repeatable, and -A, for the commands that read
// several namespaces.
type namespaceFlags struct {
	names []string
	all   bool
}

func addNamespaceFlags(cmd *cobra.Command, ns *namespaceFlags, usage string) {
	cmd.Flags().StringSliceVarP(&ns.names, "namespace", "n", nil, usage)
	cmd.Flags().BoolVarP(&ns.all, "all-namespaces", "A", false,
		"Read every namespace, and fail rather than fall back when your access doesn't allow that")
	cmd.MarkFlagsMutuallyExclusive("namespace", "all-namespaces")
}

// list is the namespaces asked for, sorted, each once.
func (ns namespaceFlags) list() []string {
	var out []string
	for _, n := range ns.names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// The -o formats every command that reports takes.
const (
	outputTable = "table"
	outputJSON  = "json"
	outputYAML  = "yaml"
)

func addOutputFlag(cmd *cobra.Command, output *string) {
	cmd.Flags().StringVarP(output, "output", "o", outputTable, "Output format: table, json, or yaml")
}

func checkOutput(output string) error {
	if output != outputTable && output != outputJSON && output != outputYAML {
		return fmt.Errorf("unknown output %q: use table, json, or yaml", output)
	}
	return nil
}

// writeOutput writes v as JSON or YAML, or calls table for the table.
func writeOutput(w io.Writer, output string, v any, table func() error) error {
	switch output {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case outputYAML:
		out, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("encoding yaml: %w", err)
		}
		_, err = w.Write(out)
		return err
	}
	return table()
}

// oneLine keeps a message from a condition or event on one table row: a
// tab or newline in it would break the columns.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
