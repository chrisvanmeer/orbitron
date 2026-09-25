{{/*
Expand the name of the chart.
*/}}
{{- define "orbitron.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to 63
bytes. If the release name contains the chart name it will be used as a full
name.
*/}}
{{- define "orbitron.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "orbitron.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "orbitron.labels" -}}
helm.sh/chart: {{ include "orbitron.chart" . }}
{{ include "orbitron.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "orbitron.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orbitron.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The application image reference: repository plus tag, defaulting the tag to the
chart appVersion (e.g. 2.0.0).
*/}}
{{- define "orbitron.image" -}}
{{- printf "%s:%s" .Values.image.repository (default (.Chart.AppVersion) .Values.image.tag) }}
{{- end }}

{{/*
ServiceAccount name to use.
*/}}
{{- define "orbitron.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "orbitron.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
True when the entrypoint needs env-based OIDC secrets.
*/}}
{{- define "orbitron.oidcSecretName" -}}
{{- if .Values.oidc.existingSecret }}
{{- .Values.oidc.existingSecret }}
{{- else if .Values.oidc.clientSecret }}
{{- printf "%s-secrets" (include "orbitron.fullname" .) }}
{{- else }}
{{- "" }}
{{- end }}
{{- end }}

{{/*
True when a generated Secret (CA bundle or inline OIDC secret) should exist.
*/}}
{{- define "orbitron.needsGeneratedSecret" -}}
{{- or .Values.env.tls.caBundle .Values.oidc.clientSecret }}
{{- end }}