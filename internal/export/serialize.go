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

package export

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// WriteYAML serializes each ManagedWorkload as a YAML document to w,
// separated by "---" document markers.
func WriteYAML(w io.Writer, workloads []v1alpha1.ManagedWorkload) error {
	for i, mw := range workloads {
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return fmt.Errorf("writing separator: %w", err)
			}
		}

		data, err := yaml.Marshal(mw)
		if err != nil {
			return fmt.Errorf("marshaling %s: %w", mw.Name, err)
		}

		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("writing %s: %w", mw.Name, err)
		}
	}
	return nil
}

// WriteFiles writes each ManagedWorkload as an individual YAML file to dir.
// Files are named <name>.yaml.
func WriteFiles(dir string, workloads []v1alpha1.ManagedWorkload) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	for _, mw := range workloads {
		path := filepath.Join(dir, mw.Name+".yaml")

		data, err := yaml.Marshal(mw)
		if err != nil {
			return fmt.Errorf("marshaling %s: %w", mw.Name, err)
		}

		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}
