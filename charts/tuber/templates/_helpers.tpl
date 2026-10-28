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
{{- $cluster := .Values.cluster -}}
cluster:
  {{- toYaml (pick $cluster "name" "provider" "region") | nindent 2 }}
  {{- with $cluster.id }}
  id: {{ . | quote }}
  {{- end }}
  {{- if $cluster.detectID }}
  detectID: true
  {{- end }}
  {{- /* The runtime default is 1h, so only a different interval is rendered; 0 is a value. */}}
  {{- if and (not (kindIs "invalid" $cluster.detectInterval)) (ne (toString $cluster.detectInterval) "1h") }}
  detectInterval: {{ $cluster.detectInterval | toString | quote }}
  {{- end }}
  {{- with $cluster.distributions }}
  distributions:
    {{- toYaml . | nindent 4 }}
  {{- end }}
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
