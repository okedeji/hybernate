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
