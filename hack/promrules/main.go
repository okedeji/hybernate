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

// Command promrules turns the PrometheusRule read from stdin into the plain
// rule file promtool tests, by writing out its spec.
//
//	go run ./hack/promrules < config/prometheus/alerts.yaml > alerts.rules.yaml
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

var documentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	manifests, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading manifests: %w", err)
	}
	for _, doc := range documentSeparator.Split(string(manifests), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var rule struct {
			Kind string         `json:"kind"`
			Spec map[string]any `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &rule); err != nil {
			return fmt.Errorf("parsing a manifest: %w", err)
		}
		if rule.Kind != "PrometheusRule" {
			continue
		}
		rules, err := yaml.Marshal(rule.Spec)
		if err != nil {
			return fmt.Errorf("writing rules: %w", err)
		}
		if _, err := out.Write(rules); err != nil {
			return fmt.Errorf("writing rules: %w", err)
		}
		return nil
	}
	return errors.New("no PrometheusRule in the input")
}
