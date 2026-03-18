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
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/export"
)

var scheme = runtime.NewScheme()

func init() {
	_ = v1alpha1.AddToScheme(scheme)
}

func main() {
	root := &cobra.Command{
		Use:   "kubectl-hybernate",
		Short: "Hybernate kubectl plugin for workload lifecycle management",
	}

	root.AddCommand(exportCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func exportCmd() *cobra.Command {
	var (
		namespace       string
		policyName      string
		outputDir       string
		classifications []string
		name            string
		includeManaged  bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export discovered workloads as ManagedWorkload YAMLs",
		Long: `Export generates ManagedWorkload YAML manifests from a WorkloadPolicy's
discovered workloads. The generated files can be committed to Git and
deployed via GitOps (ArgoCD, Flux, etc).

By default, already-managed and ignored workloads are skipped. Use
--include-managed to include workloads that already have a ManagedWorkload CR.

Examples:
  # Export all unmanaged workloads from a policy to stdout
  kubectl hybernate export --policy staging-policy -n staging

  # Export only idle workloads to a directory (one file per workload)
  kubectl hybernate export --policy staging-policy -n staging --idle --output ./manifests/

  # Export a specific workload
  kubectl hybernate export --policy staging-policy -n staging --name my-app

  # Include already-managed workloads (for auto-manage to GitOps graduation)
  kubectl hybernate export --policy staging-policy -n staging --include-managed --output ./manifests/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if policyName == "" {
				return fmt.Errorf("--policy is required")
			}

			k8s, err := buildClient()
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}

			var policy v1alpha1.WorkloadPolicy
			key := client.ObjectKey{Namespace: namespace, Name: policyName}
			if err := k8s.Get(context.Background(), key, &policy); err != nil {
				return fmt.Errorf("fetching WorkloadPolicy %s/%s: %w", namespace, policyName, err)
			}

			filter := export.Filter{
				Name:           name,
				IncludeManaged: includeManaged,
			}
			for _, c := range classifications {
				filter.Classifications = append(filter.Classifications, v1alpha1.Classification(c))
			}

			result := export.Generate(&policy, filter)

			for _, s := range result.Skipped {
				fmt.Fprintf(os.Stderr, "Skipping %s/%s: %s\n", s.Kind, s.Name, s.Reason)
			}

			if len(result.Workloads) == 0 {
				fmt.Fprintln(os.Stderr, "No workloads to export.")
				return nil
			}

			if outputDir != "" {
				if err := export.WriteFiles(outputDir, result.Workloads); err != nil {
					return fmt.Errorf("writing files: %w", err)
				}
				fmt.Fprintf(os.Stderr, "Wrote %d manifests to %s\n", len(result.Workloads), outputDir)
				return nil
			}

			return export.WriteYAML(os.Stdout, result.Workloads)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace of the WorkloadPolicy")
	cmd.Flags().StringVar(&policyName, "policy", "", "Name of the WorkloadPolicy to export from")
	cmd.Flags().StringVarP(&outputDir, "output", "o", "", "Directory to write individual YAML files (stdout if omitted)")
	cmd.Flags().StringVar(&name, "name", "", "Export only the workload with this name")
	cmd.Flags().StringSliceVar(
		&classifications, "classification", nil, "Filter by classification (Active, Idle, Wasteful)",
	)
	cmd.Flags().BoolVar(
		&includeManaged, "include-managed", false, "Include workloads that already have a ManagedWorkload CR",
	)

	return cmd
}

func buildClient() (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	return client.New(config, client.Options{Scheme: scheme})
}
