{{/* The chart's name. */}}
{{- define "kardigan.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* The prefix for resource names. A release named after the chart isn't doubled up. */}}
{{- define "kardigan.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "kardigan.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "kardigan.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/* Labels that select the operator's pods. These can't change after install. */}}
{{- define "kardigan.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kardigan.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Labels for every resource. */}}
{{- define "kardigan.labels" -}}
{{ include "kardigan.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* The operator's service account. */}}
{{- define "kardigan.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kardigan.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
