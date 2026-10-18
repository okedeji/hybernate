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

// Package docs_test proves that every ManagedWorkload the docs and samples
// show, whole or in part, is one the API server accepts, and that every set
// of Helm values the docs show is one the chart accepts, so a reader who
// copies one never meets a validation error.
package docs_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const repoRoot = "../.."

// snippetMarker, on the line before a fenced block, marks a deliberately
// partial example that isn't meant to be applied as it stands.
const snippetMarker = "<!-- snippet -->"

// manifest is one YAML document, where a reader would find it, and the file
// name the docs give its block, if any.
type manifest struct {
	source string
	title  string
	data   []byte
}

func TestDocsManifestsAreValid(t *testing.T) {
	manifests := append(markdownManifests(t), sampleManifests(t)...)
	workloads := managedWorkloads(t, manifests, specFields(t))
	require.NotEmpty(t, workloads, "the docs show at least one ManagedWorkload")

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "starting envtest; run through `make test` so KUBEBUILDER_ASSETS is set")
	t.Cleanup(func() {
		assert.NoError(t, env.Stop())
	})

	c, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, w := range workloads {
		t.Run(w.source, func(t *testing.T) {
			obj := w.object
			if obj.GetNamespace() == "" {
				obj.SetNamespace(metav1.NamespaceDefault)
			}
			ensureNamespace(ctx, t, c, obj.GetNamespace())

			err := c.Create(ctx, obj, client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict))
			assert.NoError(t, err, "%s: the API server rejects this ManagedWorkload", w.source)
		})
	}
}

type workload struct {
	source string
	object *unstructured.Unstructured
}

// managedWorkloads picks the ManagedWorkloads out of the manifests, along
// with the partial ones the docs show, wrapped into whole ones. A document
// that doesn't parse fails the test only when it looks like a
// ManagedWorkload, since the docs also show Helm values and other YAML.
func managedWorkloads(t *testing.T, manifests []manifest, fields map[string]bool) []workload {
	t.Helper()
	var out []workload
	for _, m := range manifests {
		var obj map[string]any
		if err := yaml.Unmarshal(m.data, &obj); err != nil {
			assert.NotContains(t, string(m.data), "kind: ManagedWorkload", "%s doesn't parse: %v", m.source, err)
			continue
		}
		if fragment, ok := specFragment(m, obj, fields); ok {
			out = append(out, workload{source: m.source, object: wrapSpec(fragment)})
			continue
		}
		if obj["kind"] != "ManagedWorkload" {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		if !assert.Equal(t, "hybernate.io/v1alpha1", u.GetAPIVersion(), "%s: apiVersion", m.source) {
			continue
		}
		out = append(out, workload{source: m.source, object: u})
	}
	return out
}

// specFragment returns the ManagedWorkload spec a partial block shows: a
// spec: block, or the spec's fields on their own, such as a dependsOn list.
// It's one when any of its fields is a ManagedWorkload spec field, so that a
// misspelt field beside a real one still fails. A block the docs title as
// another file, such as an Argo CD Application's spec, is never one.
func specFragment(m manifest, obj map[string]any, fields map[string]bool) (map[string]any, bool) {
	if _, ok := obj["kind"]; ok {
		return nil, false
	}
	if _, ok := obj["apiVersion"]; ok {
		return nil, false
	}
	if m.title != "" && m.title != "managedworkload.yaml" {
		return nil, false
	}
	candidate := obj
	if s, ok := obj["spec"].(map[string]any); ok && len(obj) == 1 {
		candidate = s
	}
	for key := range candidate {
		if fields[key] {
			return candidate, true
		}
	}
	return nil, false
}

// wrapSpec makes a whole ManagedWorkload of a partial spec, filling in only
// the fields the API server requires.
func wrapSpec(fragment map[string]any) *unstructured.Unstructured {
	s := maps.Clone(fragment)
	if _, ok := s["target"]; !ok {
		s["target"] = map[string]any{"kind": "Deployment", "name": "my-api"}
	}
	if _, ok := s["prediction"]; !ok {
		s["prediction"] = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "hybernate.io/v1alpha1",
		"kind":       "ManagedWorkload",
		"metadata":   map[string]any{"name": "docs-snippet"},
		"spec":       s,
	}}
}

// specFields returns the names of the ManagedWorkload spec's fields. They're
// read from the CRD, so that a field added to the API is recognised in the
// docs without changing this test.
func specFields(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "config", "crd", "bases", "hybernate.io_managedworkloads.yaml"))
	require.NoError(t, err)
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties map[string]any `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &crd))
	require.NotEmpty(t, crd.Spec.Versions)
	fields := map[string]bool{}
	for name := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties {
		fields[name] = true
	}
	require.NotEmpty(t, fields, "the CRD's spec has fields")
	return fields
}

// TestDocsHelmValuesAreValid checks the docs' Helm values against the
// chart's values.schema.json, as helm install would, which also refuses a
// value the chart doesn't have, such as a misspelt one.
func TestDocsHelmValuesAreValid(t *testing.T) {
	chart := filepath.Join(repoRoot, "charts", "hybernate")

	data, err := os.ReadFile(filepath.Join(chart, "values.schema.json"))
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	definitions, _ := raw["definitions"].(map[string]any)
	data, err = json.Marshal(inlineRefs(t, raw, definitions))
	require.NoError(t, err)
	var schema spec.Schema
	require.NoError(t, json.Unmarshal(data, &schema))

	data, err = os.ReadFile(filepath.Join(chart, "values.yaml"))
	require.NoError(t, err)
	var defaults map[string]any
	require.NoError(t, yaml.Unmarshal(data, &defaults))

	var checked int
	for _, m := range markdownManifests(t) {
		var values map[string]any
		if yaml.Unmarshal(m.data, &values) != nil || !isChartValues(m, values, defaults, &schema) {
			continue
		}
		checked++
		t.Run(m.source, func(t *testing.T) {
			assert.NoError(t, validate.AgainstSchema(&schema, values, strfmt.Default),
				"%s: the chart's schema rejects these values", m.source)
		})
	}
	require.NotZero(t, checked, "the docs show at least one set of Helm values")
}

// inlineRefs replaces each "$ref" to the schema's definitions with the
// definition, since the validator doesn't follow references, as helm does.
func inlineRefs(t *testing.T, node any, definitions map[string]any) any {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			def, found := definitions[strings.TrimPrefix(ref, "#/definitions/")]
			require.True(t, found, "the chart's schema has no %s", ref)
			return inlineRefs(t, def, definitions)
		}
		out := make(map[string]any, len(n))
		for k, v := range n {
			out[k] = inlineRefs(t, v, definitions)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inlineRefs(t, v, definitions)
		}
		return out
	default:
		return node
	}
}

var valuesFile = regexp.MustCompile(`^values(-[\w.-]+)?\.yaml$`)

// isChartValues reports whether a block is Hybernate's Helm values: one the
// docs title values.yaml or values-<something>.yaml, or an untitled one
// whose every key is one of the chart's. Another chart's values, such as
// the cluster autoscaler's, are titled differently.
func isChartValues(m manifest, values, defaults map[string]any, schema *spec.Schema) bool {
	if valuesFile.MatchString(m.title) {
		return true
	}
	if m.title != "" || len(values) == 0 {
		return false
	}
	for key := range values {
		if _, ok := defaults[key]; ok {
			continue
		}
		if _, ok := schema.Properties[key]; ok {
			continue
		}
		return false
	}
	return true
}

func markdownManifests(t *testing.T) []manifest {
	t.Helper()
	files := []string{filepath.Join(repoRoot, "README.md")}
	err := filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
			files = append(files, path)
		}
		return err
	})
	require.NoError(t, err)

	out := make([]manifest, 0, len(files))
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		rel, err := filepath.Rel(repoRoot, file)
		require.NoError(t, err)
		for i, b := range yamlBlocks(string(data)) {
			docs := splitDocuments(t, fmt.Sprintf("%s block %d", rel, i+1), b.body)
			for j := range docs {
				docs[j].title = b.title
			}
			out = append(out, docs...)
		}
	}
	return out
}

func sampleManifests(t *testing.T) []manifest {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "config", "samples", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	out := make([]manifest, 0, len(files))
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		rel, err := filepath.Rel(repoRoot, file)
		require.NoError(t, err)
		out = append(out, splitDocuments(t, rel, data)...)
	}
	return out
}

type block struct {
	title string
	body  []byte
}

var fenceTitle = regexp.MustCompile(`title="([^"]*)"`)

// yamlBlocks returns the fenced yaml blocks of a Markdown page, in order,
// leaving out those marked as snippets. Fences may be indented, as they are
// inside admonitions and tabs, so each block is dedented by its fence's
// indentation.
func yamlBlocks(page string) []block {
	var (
		blocks   []block
		current  *bytes.Buffer
		title    string
		indent   string
		skip     bool
		previous string
	)
	scanner := bufio.NewScanner(strings.NewReader(page))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case current == nil && isYAMLFence(trimmed):
			current = &bytes.Buffer{}
			title = ""
			if match := fenceTitle.FindStringSubmatch(trimmed); match != nil {
				title = match[1]
			}
			indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			skip = previous == snippetMarker
		case current != nil && trimmed == "```":
			if !skip {
				blocks = append(blocks, block{title: title, body: current.Bytes()})
			}
			current = nil
		case current != nil:
			current.WriteString(strings.TrimPrefix(line, indent))
			current.WriteByte('\n')
		}
		if trimmed != "" {
			previous = trimmed
		}
	}
	return blocks
}

// isYAMLFence reports whether a line opens a yaml block, with or without
// attributes such as a title.
func isYAMLFence(line string) bool {
	lang, _, _ := strings.Cut(line, " ")
	return lang == "```yaml" || lang == "```yml"
}

func splitDocuments(t *testing.T, source string, data []byte) []manifest {
	t.Helper()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []manifest
	for n := 1; ; n++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err, "reading %s", source)
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		name := source
		if n > 1 {
			name = fmt.Sprintf("%s document %d", source, n)
		}
		out = append(out, manifest{source: name, data: doc})
	}
}

func ensureNamespace(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "creating namespace %s", name)
	}
}

func TestYAMLBlocks(t *testing.T) {
	tests := []struct {
		name string
		page string
		want []block
	}{
		{
			name: "a plain block",
			page: "text\n```yaml\nkind: A\n```\n",
			want: []block{{body: []byte("kind: A\n")}},
		},
		{
			name: "a block with a title",
			page: "```yaml title=\"x.yaml\" linenums=\"1\"\nkind: A\n```\n",
			want: []block{{title: "x.yaml", body: []byte("kind: A\n")}},
		},
		{
			name: "an indented block, inside a tab",
			page: "=== \"Tab\"\n\n    ```yaml\n    kind: A\n    spec: {}\n    ```\n",
			want: []block{{body: []byte("kind: A\nspec: {}\n")}},
		},
		{
			name: "a block marked as a snippet",
			page: snippetMarker + "\n\n```yaml\nkind: A\n```\n```yaml\nkind: B\n```\n",
			want: []block{{body: []byte("kind: B\n")}},
		},
		{
			name: "other languages",
			page: "```bash\nkubectl get managedworkloads\n```\n```\nplain\n```\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, yamlBlocks(tt.page))
		})
	}
}

func TestSpecFragment(t *testing.T) {
	fields := map[string]bool{"idlePolicy": true, "dependsOn": true}
	tests := []struct {
		name  string
		title string
		block string
		want  bool
	}{
		{name: "a spec block", block: "spec:\n  idlePolicy: {}\n", want: true},
		{
			name:  "a spec block titled as a ManagedWorkload",
			title: "managedworkload.yaml",
			block: "spec:\n  idlePolicy: {}\n",
			want:  true,
		},
		{name: "spec fields on their own", block: "dependsOn: []\n", want: true},
		{name: "a misspelt field beside a real one", block: "spec:\n  idlePolicy: {}\n  idlePolcy: {}\n", want: true},
		{name: "another resource's spec", title: "application.yaml", block: "spec:\n  idlePolicy: {}\n"},
		{name: "a spec of another kind", block: "spec:\n  syncPolicy: {}\n"},
		{name: "a whole object", block: "kind: ManagedWorkload\nspec:\n  idlePolicy: {}\n"},
		{name: "a status block", block: "status:\n  cost: {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var obj map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(tt.block), &obj))
			_, got := specFragment(manifest{title: tt.title}, obj, fields)
			assert.Equal(t, tt.want, got)
		})
	}
}
