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

// Command rbaccheck fails when what the Helm chart grants the operator
// differs from config/rbac/role.yaml, which controller-gen writes from the
// kubebuilder markers. The chart keeps its own copy of the rules, split
// between a ClusterRole and per-namespace Roles, so nothing else keeps the
// two in step. It reads the rendered chart from stdin and compares the
// rules of every Role and ClusterRole whose name ends in -manager.
//
//	helm template hybernate charts/hybernate | go run ./hack/rbaccheck config/rbac/role.yaml
package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: helm template ... | rbaccheck config/rbac/role.yaml")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(chart io.Reader, rolePath string) error {
	generated, err := os.ReadFile(rolePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", rolePath, err)
	}
	want, err := grants(generated, func(string) bool { return true })
	if err != nil {
		return fmt.Errorf("reading %s: %w", rolePath, err)
	}
	rendered, err := io.ReadAll(chart)
	if err != nil {
		return fmt.Errorf("reading the rendered chart: %w", err)
	}
	got, err := grants(rendered, func(name string) bool { return strings.HasSuffix(name, "-manager") })
	if err != nil {
		return fmt.Errorf("reading the rendered chart: %w", err)
	}

	missing, extra := difference(want, got), difference(got, want)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the chart's operator RBAC differs from %s; "+
		"update hybernate.managerRules in charts/hybernate/templates/_helpers.tpl\n", rolePath)
	for _, g := range missing {
		fmt.Fprintf(&b, "  missing from the chart: %s\n", g)
	}
	for _, g := range extra {
		fmt.Fprintf(&b, "  only in the chart:      %s\n", g)
	}
	return fmt.Errorf("%s", strings.TrimSuffix(b.String(), "\n"))
}

var documentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// grants expands the rules of every Role and ClusterRole that named selects
// into one "verb group/resource" per grant, sorted and without duplicates,
// so rules grouped differently compare equal.
func grants(manifests []byte, named func(string) bool) ([]string, error) {
	var out []string
	for _, doc := range documentSeparator.Split(string(manifests), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			return nil, fmt.Errorf("parsing a manifest: %w", err)
		}
		if (role.Kind != "Role" && role.Kind != "ClusterRole") || !named(role.Metadata.Name) {
			continue
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						out = append(out, fmt.Sprintf("%s %s/%s", verb, group, resource))
					}
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// difference is what's in a and not in b, both sorted.
func difference(a, b []string) []string {
	var out []string
	for _, g := range a {
		if _, found := slices.BinarySearch(b, g); !found {
			out = append(out, g)
		}
	}
	return out
}
