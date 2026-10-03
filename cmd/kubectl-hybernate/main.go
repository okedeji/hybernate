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
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = metricsv1beta1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
}

func main() {
	root := &cobra.Command{
		Use:   "kubectl-hybernate",
		Short: "Hybernate kubectl plugin for workload lifecycle management",
	}

	root.AddCommand(enableCmd(), scanCmd(), wakeCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// buildClient returns a client for the kubeconfig's current context and that
// context's namespace, which is what kubectl uses when -n isn't given.
func buildClient() (client.Client, string, error) {
	c, namespace, _, err := buildClientFor("")
	return c, namespace, err
}

// buildClientFor returns a client for a kubeconfig context, the current one
// if contextName is empty, with the context's namespace and name.
func buildClientFor(contextName string) (c client.Client, namespace, name string, err error) {
	config, namespace, name, err := kubeConfigFor(contextName)
	if err != nil {
		return nil, "", name, err
	}
	c, err = client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", name, fmt.Errorf("creating client: %w", err)
	}
	return c, namespace, name, nil
}

// kubeConfigFor loads a kubeconfig context, the current one if contextName
// is empty, with the context's namespace and name.
func kubeConfigFor(contextName string) (config *rest.Config, namespace, name string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: contextName})
	name = contextName
	if raw, err := loader.RawConfig(); err == nil && name == "" {
		name = raw.CurrentContext
	}
	config, err = loader.ClientConfig()
	if err != nil {
		return nil, "", name, fmt.Errorf("loading kubeconfig: %w", err)
	}
	namespace, _, err = loader.Namespace()
	if err != nil {
		return nil, "", name, fmt.Errorf("reading kubeconfig namespace: %w", err)
	}
	return config, namespace, name, nil
}
