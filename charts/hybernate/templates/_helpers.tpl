{{/*
Chart name, truncated to 63 chars.
*/}}
{{- define "hybernate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to 63 chars.
*/}}
{{- define "hybernate.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "hybernate.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "hybernate.labels" -}}
helm.sh/chart: {{ include "hybernate.chart" . }}
{{ include "hybernate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used by Deployment and Service.
*/}}
{{- define "hybernate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hybernate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Doorman name and labels. Distinct from the operator's, so the
operator's Service and NetworkPolicy don't select doorman pods.
*/}}
{{- define "hybernate.doormanName" -}}
{{- printf "%s-doorman" (include "hybernate.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "hybernate.doormanSelectorLabels" -}}
app.kubernetes.io/name: {{ include "hybernate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: doorman
{{- end }}

{{- define "hybernate.doormanLabels" -}}
helm.sh/chart: {{ include "hybernate.chart" . }}
{{ include "hybernate.doormanSelectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "hybernate.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "hybernate.fullname" . }}
{{- end }}
{{- end }}

{{/*
Controller image.
*/}}
{{- define "hybernate.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
The operator's rules on namespaced resources: a ClusterRole's, or with
watchNamespaces, a Role's in each of them.
*/}}
{{- define "hybernate.managerRules" -}}
- apiGroups: [""]
  resources: [configmaps]
  verbs: [get]
- apiGroups: [""]
  resources: [events]
  verbs: [create, patch]
- apiGroups: [events.k8s.io]
  resources: [events]
  verbs: [create, patch]
- apiGroups: [""]
  resources: [services]
  verbs: [get, list, watch]
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list]
- apiGroups: [discovery.k8s.io]
  resources: [endpointslices]
  verbs: [create, delete, get, list, patch, update, watch]
- apiGroups: [""]
  resources: [persistentvolumeclaims]
  verbs: [get, list, watch]
- apiGroups: [apps]
  resources: [deployments, statefulsets]
  verbs: [get, list, watch]
- apiGroups: [apps]
  resources: [deployments/scale, statefulsets/scale]
  verbs: [get, update]
- apiGroups: [autoscaling]
  resources: [horizontalpodautoscalers]
  verbs: [get, list, watch]
- apiGroups: [keda.sh]
  resources: [scaledobjects]
  verbs: [get, list, patch, watch]
- apiGroups: [hybernate.io]
  resources: [managedworkloads]
  verbs: [create, delete, get, list, patch, update, watch]
- apiGroups: [hybernate.io]
  resources: [managedworkloads/finalizers]
  verbs: [update]
- apiGroups: [hybernate.io]
  resources: [managedworkloads/status]
  verbs: [get, patch, update]
- apiGroups: [metrics.k8s.io]
  resources: [pods]
  verbs: [get, list]
{{- end }}

{{/*
The operator's rules on cluster-scoped resources, which only a ClusterRole
can grant: reading namespaces, for their labels, and nodes, for their
instance types.
*/}}
{{- define "hybernate.managerClusterRules" -}}
- apiGroups: [""]
  resources: [namespaces, nodes]
  verbs: [get, list, watch]
{{- end }}

{{/*
The doorman's rules, all on namespaced resources.
*/}}
{{- define "hybernate.doormanRules" -}}
- apiGroups: [hybernate.io]
  resources: [managedworkloads]
  verbs: [get, list, watch, patch]
- apiGroups: [discovery.k8s.io]
  resources: [endpointslices]
  verbs: [get, list, watch]
- apiGroups: ["", events.k8s.io]
  resources: [events]
  verbs: [create, patch]
{{- end }}
