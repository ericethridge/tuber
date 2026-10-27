{{- define "tuber.fullname" -}}
{{- if eq .Release.Name "tuber" -}}
tuber
{{- else -}}
{{- printf "%s-tuber" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "tuber.labels" -}}
app.kubernetes.io/name: tuber
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}

{{- define "tuber.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "tuber.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "tuber.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default (printf "v%s" .Chart.AppVersion) .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "tuber.config" -}}
{{- range .Values.rules -}}
{{- range .namespaces -}}
{{- if not (has . $.Values.watchNamespaces) -}}
{{- fail (printf "rule namespace %q must be listed in watchNamespaces" .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
cluster:
  {{- toYaml .Values.cluster | nindent 2 }}
watchNamespaces:
  {{- toYaml .Values.watchNamespaces | nindent 2 }}
dryRun: {{ .Values.dryRun }}
doit:
  serviceAccountTokenFile: /var/run/secrets/doit/service-account-token
  {{- toYaml .Values.doit | nindent 2 }}
rollup:
  {{- toYaml .Values.rollup | nindent 2 }}
rules:
  {{- toYaml .Values.rules | nindent 2 }}
{{- end -}}
