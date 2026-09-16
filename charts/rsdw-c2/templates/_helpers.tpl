{{- define "rsdwc2.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "rsdwc2.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "rsdwc2.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- define "rsdwc2.labels" -}}
app.kubernetes.io/name: {{ include "rsdwc2.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: Helm
{{- end -}}
{{- define "rsdwc2.image" -}}
{{- if .Values.image.tag -}}{{ .Values.image.repository }}:{{ .Values.image.tag }}{{- else -}}{{ .Values.image.repository }}:{{ .Chart.AppVersion }}{{- end -}}
{{- end -}}
