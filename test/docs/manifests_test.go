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
// show is one the API server accepts, so a reader who copies one never meets
// a validation error.
package docs_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const repoRoot = "../.."

// snippetMarker, on the line before a fenced block, marks a deliberately
// partial ManagedWorkload that isn't meant to be applied as it stands.
const snippetMarker = "<!-- snippet -->"

// manifest is one YAML document, and where a reader would find it.
type manifest struct {
	source string
	data   []byte
}

func TestDocsManifestsAreValid(t *testing.T) {
	manifests := append(markdownManifests(t), sampleManifests(t)...)
	workloads := managedWorkloads(t, manifests)
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

// managedWorkloads picks the ManagedWorkloads out of the manifests. A
// document that doesn't parse fails the test only when it looks like a
// ManagedWorkload, since the docs also show Helm values and other YAML.
func managedWorkloads(t *testing.T, manifests []manifest) []workload {
	t.Helper()
	var out []workload
	for _, m := range manifests {
		var obj map[string]any
		if err := yaml.Unmarshal(m.data, &obj); err != nil {
			assert.NotContains(t, string(m.data), "kind: ManagedWorkload", "%s doesn't parse: %v", m.source, err)
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
		for i, block := range yamlBlocks(string(data)) {
			out = append(out, splitDocuments(t, fmt.Sprintf("%s block %d", rel, i+1), block)...)
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

// yamlBlocks returns the fenced yaml blocks of a Markdown page, in order,
// leaving out those marked as snippets. Fences may be indented, as they are
// inside admonitions and tabs, so each block is dedented by its fence's
// indentation.
func yamlBlocks(page string) [][]byte {
	var (
		blocks   [][]byte
		current  *bytes.Buffer
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
			indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			skip = previous == snippetMarker
		case current != nil && trimmed == "```":
			if !skip {
				blocks = append(blocks, current.Bytes())
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
		want []string
	}{
		{
			name: "a plain block",
			page: "text\n```yaml\nkind: A\n```\n",
			want: []string{"kind: A\n"},
		},
		{
			name: "a block with a title",
			page: "```yaml title=\"x.yaml\" linenums=\"1\"\nkind: A\n```\n",
			want: []string{"kind: A\n"},
		},
		{
			name: "an indented block, inside a tab",
			page: "=== \"Tab\"\n\n    ```yaml\n    kind: A\n    spec: {}\n    ```\n",
			want: []string{"kind: A\nspec: {}\n"},
		},
		{
			name: "a block marked as a snippet",
			page: snippetMarker + "\n\n```yaml\nkind: A\n```\n```yaml\nkind: B\n```\n",
			want: []string{"kind: B\n"},
		},
		{
			name: "other languages",
			page: "```bash\nkubectl get managedworkloads\n```\n```\nplain\n```\n",
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := yamlBlocks(tt.page)
			got := make([]string, 0, len(blocks))
			for _, b := range blocks {
				got = append(got, string(b))
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
