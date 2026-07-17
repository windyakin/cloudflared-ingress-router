{{- define "cloudflared-ingress-router.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cloudflared-ingress-router.fullname" -}}
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

{{- define "cloudflared-ingress-router.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cloudflared-ingress-router.labels" -}}
helm.sh/chart: {{ include "cloudflared-ingress-router.chart" . }}
{{ include "cloudflared-ingress-router.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "cloudflared-ingress-router.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cloudflared-ingress-router.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cloudflared-ingress-router.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cloudflared-ingress-router.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "cloudflared-ingress-router.secretName" -}}
{{- if .Values.cloudflare.existingSecret }}
{{- .Values.cloudflare.existingSecret }}
{{- else }}
{{- include "cloudflared-ingress-router.fullname" . }}
{{- end }}
{{- end }}
