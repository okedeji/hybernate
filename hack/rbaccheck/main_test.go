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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const generated = `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
- apiGroups: [""]
  resources: [configmaps, services]
  verbs: [get]
- apiGroups: [apps]
  resources: [deployments/scale]
  verbs: [get, update]
`

func writeRole(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "role.yaml")
	require.NoError(t, os.WriteFile(path, []byte(generated), 0o600))
	return path
}

func TestRunAcceptsTheSameGrantsSplitAcrossRoles(t *testing.T) {
	chart := `---
kind: ClusterRole
metadata: {name: hybernate-manager}
rules:
- apiGroups: [""]
  resources: [services]
  verbs: [get]
---
kind: Role
metadata: {name: hybernate-manager, namespace: shop}
rules:
- apiGroups: [""]
  resources: [configmaps]
  verbs: [get]
- apiGroups: [apps]
  resources: [deployments/scale]
  verbs: [update, get]
---
kind: Role
metadata: {name: hybernate-leader-election}
rules:
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  verbs: [get, create, update]
`
	assert.NoError(t, run(strings.NewReader(chart), writeRole(t)))
}

func TestRunReportsDrift(t *testing.T) {
	chart := `---
kind: ClusterRole
metadata: {name: hybernate-manager}
rules:
- apiGroups: [""]
  resources: [configmaps, services]
  verbs: [get, update]
`
	err := run(strings.NewReader(chart), writeRole(t))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing from the chart: get apps/deployments/scale")
	assert.Contains(t, err.Error(), "only in the chart:      update /configmaps")
}
