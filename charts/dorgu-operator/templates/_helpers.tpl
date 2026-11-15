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

{{/*
The apply ticket lifetime, validated the way the operator validates --ticket-ttl: a
Go duration (a number and a unit, ns us µs ms s m h, one or more), positive and at
most 24h. A value the operator would refuse at startup fails the chart here instead,
so `helm install` and `helm upgrade` stop with this message and no pod crash-loops.
The fallback keeps a --reuse-values upgrade from an older release rendering the
default, never an empty flag.
*/}}
{{- define "dorgu-operator.ticketTTL" -}}
{{- $raw := dig "ticketTTL" "15m" (.Values.remediation | default dict) -}}
{{- if not (kindIs "string" $raw) -}}
{{- fail (printf "remediation.ticketTTL must be a duration string such as 15m or 1h30m, got %v (%s): a bare number has no unit" $raw (kindOf $raw)) -}}
{{- end -}}
{{- if not (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$" $raw) -}}
{{- fail (printf "remediation.ticketTTL %q is not a duration such as 15m, 90s or 1h30m (a number and a unit: ns, us, ms, s, m, h)" $raw) -}}
{{- end -}}
{{- $units := dict "ns" 0.000000001 "us" 0.000001 "µs" 0.000001 "ms" 0.001 "s" 1.0 "m" 60.0 "h" 3600.0 -}}
{{- $seconds := 0.0 -}}
{{- range $part := regexFindAll "[0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h)" $raw -1 -}}
{{- $number := regexFind "^[0-9]+(\\.[0-9]+)?" $part | float64 -}}
{{- $unit := regexFind "(ns|us|µs|ms|s|m|h)$" $part -}}
{{- $seconds = addf $seconds (mulf $number (get $units $unit)) -}}
{{- end -}}
{{- if le $seconds 0.0 -}}
{{- fail (printf "remediation.ticketTTL %q must be positive: a ticket that lives no time is refused by the operator at startup" $raw) -}}
{{- end -}}
{{- if gt $seconds 86400.0 -}}
{{- fail (printf "remediation.ticketTTL %q is longer than 24h, the most the operator accepts" $raw) -}}
{{- end -}}
{{- $raw -}}
{{- end -}}

{{/*
How long a value a person rolled back stays withheld, validated the way the operator
validates --rolled-back-value-ttl: a Go duration, positive and at most 2160h (90 days).
A value the operator would refuse at startup fails the chart here instead. The fallback
keeps a --reuse-values upgrade from an older release rendering the default, never an
empty flag.
*/}}
{{- define "dorgu-operator.rolledBackValueTTL" -}}
{{- $raw := dig "rolledBackValueTTL" "168h" (.Values.remediation | default dict) -}}
{{- if not (kindIs "string" $raw) -}}
{{- fail (printf "remediation.rolledBackValueTTL must be a duration string such as 168h or 48h, got %v (%s): a bare number has no unit" $raw (kindOf $raw)) -}}
{{- end -}}
{{- if not (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$" $raw) -}}
{{- fail (printf "remediation.rolledBackValueTTL %q is not a duration such as 168h, 90m or 36h30m (a number and a unit: ns, us, ms, s, m, h)" $raw) -}}
{{- end -}}
{{- $units := dict "ns" 0.000000001 "us" 0.000001 "µs" 0.000001 "ms" 0.001 "s" 1.0 "m" 60.0 "h" 3600.0 -}}
{{- $seconds := 0.0 -}}
{{- range $part := regexFindAll "[0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h)" $raw -1 -}}
{{- $number := regexFind "^[0-9]+(\\.[0-9]+)?" $part | float64 -}}
{{- $unit := regexFind "(ns|us|µs|ms|s|m|h)$" $part -}}
{{- $seconds = addf $seconds (mulf $number (get $units $unit)) -}}
{{- end -}}
{{- if le $seconds 0.0 -}}
{{- fail (printf "remediation.rolledBackValueTTL %q must be positive: a memory that lapses before it begins is refused by the operator at startup" $raw) -}}
{{- end -}}
{{- if gt $seconds 7776000.0 -}}
{{- fail (printf "remediation.rolledBackValueTTL %q is longer than 2160h (90 days), the most the operator accepts" $raw) -}}
{{- end -}}
{{- $raw -}}
{{- end -}}

{{/*
Labels of the admission policies and bindings. They carry nothing that changes from release
to release (no chart version), so config/admission/policies.yaml, rendered from the same
template for the kustomize install and the envtest suites, stays equal to the chart's.
*/}}
{{- define "dorgu-operator.admissionLabels" -}}
app.kubernetes.io/name: {{ include "dorgu-operator.name" . }}
app.kubernetes.io/component: admission
{{- end }}
