{{/* Chart name. */}}
{{- define "hangar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "hangar.fullname" -}}
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

{{- define "hangar.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "hangar.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "hangar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "hangar.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "hangar.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "hangar.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ $tag }}
{{- end -}}
{{- end }}

{{/* The database driver from the configuration (default sqlite). */}}
{{- define "hangar.dbDriver" -}}
{{- $db := default dict .Values.config.database -}}
{{- default "sqlite" $db.driver -}}
{{- end }}

{{/* Fail early on combinations that cannot work. */}}
{{- define "hangar.validate" -}}
{{- if not .Values.publicURL }}
{{- fail "publicURL is required: the URL clients use to reach Hangar, such as https://hangar.example.com" }}
{{- end }}
{{- if .Values.persistence.enabled }}
{{- if or (ne (int .Values.web.replicas) 1) (ne (int .Values.worker.replicas) 0) .Values.web.autoscaling.enabled .Values.worker.autoscaling.enabled }}
{{- fail "persistence.enabled runs one Pod in role all (SQLite and the filesystem Blob store), so set web.replicas: 1 and worker.replicas: 0 with autoscaling off, or turn persistence off and use Postgres and S3" }}
{{- end }}
{{- else }}
{{- if eq (include "hangar.dbDriver" .) "sqlite" }}
{{- fail "persistence.enabled is false but the database is SQLite; set config.database.driver: postgres (with HANGAR_DATABASE_DSN in secrets) or turn persistence on" }}
{{- end }}
{{- $storage := default dict .Values.config.storage }}
{{- if ne (default "fs" $storage.backend) "s3" }}
{{- fail "persistence.enabled is false but the Blob store is the filesystem; set config.storage.backend: s3, or turn persistence on" }}
{{- end }}
{{- if lt (int .Values.web.replicas) 1 }}
{{- fail "web.replicas must be at least 1" }}
{{- end }}
{{- end }}
{{- end }}

{{/* hangar.yaml: the user's config with the chart's keys on top. */}}
{{- define "hangar.config" -}}
{{- $server := dict "public_url" .Values.publicURL "listen" (printf ":%d" (int .Values.service.port)) }}
{{- $telemetry := dict }}
{{- if .Values.metrics.enabled }}
{{- $_ := set $telemetry "metrics" "prometheus" }}
{{- $_ := set $telemetry "prometheus_listen" (printf ":%d" (int .Values.metrics.port)) }}
{{- end }}
{{- $chart := dict "server" $server "telemetry" $telemetry }}
{{- $merged := mustMergeOverwrite (deepCopy .Values.config) $chart }}
{{- toYaml $merged }}
{{- end }}

{{/* The name of the Secret with user-supplied variables. */}}
{{- define "hangar.secretName" -}}
{{- if .Values.existingSecret }}{{ .Values.existingSecret }}{{ else }}{{ include "hangar.fullname" . }}{{ end }}
{{- end }}

{{/* Whether the chart generates the Instance key and admin password. */}}
{{- define "hangar.generate" -}}
{{- if and .Values.generateSecrets (not .Values.existingSecret) (not (hasKey .Values.secrets "HANGAR_SECURITY_INSTANCE_KEY")) }}true{{ end }}
{{- end }}

{{/*
Pod template shared by the all, web and worker Deployments.
Takes a dict: ctx (the root context), component, role, values (web or worker).
*/}}
{{- define "hangar.podTemplate" -}}
{{- $ := .ctx }}
{{- $v := .values }}
metadata:
  labels:
    {{- include "hangar.selectorLabels" $ | nindent 4 }}
    app.kubernetes.io/component: {{ .component }}
    {{- with $v.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  annotations:
    checksum/config: {{ include "hangar.config" $ | sha256sum }}
    checksum/secret: {{ toYaml $.Values.secrets | sha256sum }}
    {{- with $v.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "hangar.serviceAccountName" $ }}
  automountServiceAccountToken: {{ $.Values.serviceAccount.automountServiceAccountToken }}
  {{- with $.Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    {{- toYaml $.Values.podSecurityContext | nindent 4 }}
  terminationGracePeriodSeconds: 45
  containers:
    - name: hangar
      image: {{ include "hangar.image" $ }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args: ["server", "--role", {{ .role | quote }}]
      env:
        - name: HANGAR_CONFIG
          value: /etc/hangar/hangar.yaml
        {{- with $.Values.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      envFrom:
        {{- if or $.Values.existingSecret $.Values.secrets }}
        - secretRef:
            name: {{ include "hangar.secretName" $ }}
        {{- end }}
        {{- if include "hangar.generate" $ }}
        - secretRef:
            name: {{ include "hangar.fullname" $ }}-generated
        {{- end }}
        {{- with $.Values.extraEnvFrom }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      ports:
        - name: http
          containerPort: {{ $.Values.service.port }}
          protocol: TCP
        {{- if $.Values.metrics.enabled }}
        - name: metrics
          containerPort: {{ $.Values.metrics.port }}
          protocol: TCP
        {{- end }}
      startupProbe:
        httpGet: {path: /healthz, port: http}
        periodSeconds: 2
        failureThreshold: 60
      livenessProbe:
        httpGet: {path: /healthz, port: http}
        periodSeconds: 10
        failureThreshold: 3
      readinessProbe:
        # Ready once the database is reachable, migrations are at the
        # expected version and the Blob store answers.
        httpGet: {path: /readyz, port: http}
        periodSeconds: 5
        failureThreshold: 3
      resources:
        {{- toYaml $v.resources | nindent 8 }}
      securityContext:
        {{- toYaml $.Values.securityContext | nindent 8 }}
      volumeMounts:
        - {name: config, mountPath: /etc/hangar, readOnly: true}
        - {name: data, mountPath: /var/lib/hangar}
        - {name: cache, mountPath: /var/cache/hangar}
        - {name: tmp, mountPath: /tmp}
        {{- with $.Values.extraVolumeMounts }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
  volumes:
    - name: config
      configMap:
        name: {{ include "hangar.fullname" $ }}
    - name: data
      {{- if $.Values.persistence.enabled }}
      persistentVolumeClaim:
        claimName: {{ default (include "hangar.fullname" $) $.Values.persistence.existingClaim }}
      {{- else }}
      emptyDir: {}
      {{- end }}
    - name: cache
      emptyDir:
        sizeLimit: {{ $.Values.cacheSizeLimit }}
    - name: tmp
      emptyDir:
        sizeLimit: {{ $.Values.tmpSizeLimit }}
    {{- with $.Values.extraVolumes }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- with $v.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
