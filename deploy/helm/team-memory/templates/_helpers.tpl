{{- define "team-memory.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "team-memory.fullname" -}}
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

{{- define "team-memory.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "team-memory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* selector labels for one component: include "team-memory.selector" (list . "api") */}}
{{- define "team-memory.selector" -}}
{{- $ := index . 0 -}}
app.kubernetes.io/name: {{ include "team-memory.name" $ }}
app.kubernetes.io/instance: {{ $.Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end }}

{{- define "team-memory.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end }}

{{- define "team-memory.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{ default (include "team-memory.fullname" .) .Values.serviceAccount.name }}
{{- else -}}
{{ default "default" .Values.serviceAccount.name }}
{{- end -}}
{{- end }}

{{- define "team-memory.dbSecret" -}}
{{- required "database.existingSecret is required: create a Secret with the database URLs first (see README)" .Values.database.existingSecret -}}
{{- end }}

{{- define "team-memory.workerEnabled" -}}
{{- if and .Values.worker.enabled .Values.embeddings.ollamaURL }}true{{ end -}}
{{- end }}

{{/* Environment shared by every long-running component. */}}
{{- define "team-memory.commonEnv" -}}
- name: TEAM_MEMORY_METRICS_LISTEN
  value: ":9090"
- name: OTEL_SERVICE_NAME
  value: {{ printf "team-memory-%s" .component | quote }}
{{- with .root.Values.tracing }}
{{- if .otlpEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .otlpEndpoint | quote }}
- name: OTEL_TRACES_SAMPLER
  value: {{ .sampler | quote }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ .samplerArg | quote }}
{{- end }}
{{- end }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ printf "k8s.namespace.name=%s,service.instance.id=$(POD_NAME)" .root.Release.Namespace | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: TEAM_MEMORY_WORKER_NAME
  value: "$(POD_NAME)"
{{- end }}

{{- define "team-memory.embeddingEnv" -}}
{{- with .Values.embeddings }}
- name: TEAM_MEMORY_OLLAMA_URL
  value: {{ .ollamaURL | quote }}
- name: TEAM_MEMORY_EMBEDDING_MODEL
  value: {{ .model | quote }}
{{- if .dimensions }}
- name: TEAM_MEMORY_EMBEDDING_DIMENSIONS
  value: {{ .dimensions | quote }}
{{- end }}
{{- end }}
{{- end }}

{{- define "team-memory.podSecurity" -}}
securityContext:
  {{- toYaml .Values.podSecurityContext | nindent 2 }}
automountServiceAccountToken: false
serviceAccountName: {{ include "team-memory.serviceAccountName" . }}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/* Git mirror settings; credential files come from mirror.existingSecret. */}}
{{- define "team-memory.mirrorEnv" -}}
- name: TEAM_MEMORY_MIRROR_REMOTE
  value: {{ .Values.mirror.remote | quote }}
- name: TEAM_MEMORY_MIRROR_BRANCH
  value: {{ .Values.mirror.branch | quote }}
- name: TEAM_MEMORY_MIRROR_CACHE_DIR
  value: /tmp/mirror
{{- if hasPrefix "ssh://" .Values.mirror.remote }}
- name: TEAM_MEMORY_MIRROR_SSH_KEY_FILE
  value: /etc/team-memory/mirror/ssh-privatekey
- name: TEAM_MEMORY_MIRROR_SSH_KNOWN_HOSTS_FILE
  value: /etc/team-memory/mirror/known_hosts
{{- else }}
- name: TEAM_MEMORY_MIRROR_TOKEN_FILE
  value: /etc/team-memory/mirror/token
- name: TEAM_MEMORY_MIRROR_TOKEN_USER
  value: {{ .Values.mirror.tokenUser | quote }}
{{- if .Values.mirror.customCA }}
- name: TEAM_MEMORY_MIRROR_CA_FILE
  value: /etc/team-memory/mirror/ca.crt
{{- end }}
{{- end }}
{{- with .Values.mirror.emailDomain }}
- name: TEAM_MEMORY_MIRROR_EMAIL_DOMAIN
  value: {{ . | quote }}
{{- end }}
{{- end }}
