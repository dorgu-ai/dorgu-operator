{{/*
Expand the name of the chart.
*/}}
{{- define "dorgu-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "dorgu-operator.fullname" -}}
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
Common labels
*/}}
{{- define "dorgu-operator.labels" -}}
helm.sh/chart: {{ include "dorgu-operator.name" . }}-{{ .Chart.Version }}
{{ include "dorgu-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "dorgu-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dorgu-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "dorgu-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dorgu-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the env var carrying the LLM API key, matching what the operator reads
for llm.provider. Fails on a provider the operator does not support.
*/}}
{{- define "dorgu-operator.llmKeyEnv" -}}
{{- $envs := dict "claude" "ANTHROPIC_API_KEY" "gemini" "GEMINI_API_KEY" -}}
{{- $env := get $envs .Values.llm.provider -}}
{{- if not $env -}}
{{- fail (printf "llm.provider %q is not supported: use \"claude\" or \"gemini\", or leave it empty to disable AI" .Values.llm.provider) -}}
{{- end -}}
{{- $env -}}
{{- end }}

{{/*
Key within the LLM Secret: llm.existingSecretKey when set, otherwise the
provider's env var name.
*/}}
{{- define "dorgu-operator.llmSecretKey" -}}
{{- .Values.llm.existingSecretKey | default (include "dorgu-operator.llmKeyEnv" .) -}}
{{- end }}
