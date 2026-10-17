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
{{- if not (has .Values.migrations.mode (list "hook" "initContainer")) }}
{{- fail (printf "migrations.mode is %q; use hook (a Helm pre-install and pre-upgrade Job) or initContainer (for Argo CD and other GitOps tools)" (toString .Values.migrations.mode)) }}
{{- end }}
{{- include "hangar.validateCache" . }}
{{- end }}

{{/*
Fail when the git pack cache can outgrow its emptyDir. The cache trims
itself to git.cache_size only after a Repo is released, and git also writes
views and staging directories next to it, so the volume needs room above
the cache: the kubelet evicts a Pod whose emptyDir passes its sizeLimit.
Only checked when the cache is on the chart's cache volume.
*/}}
{{- define "hangar.validateCache" -}}
{{- $git := default dict .Values.config.git }}
{{- $dir := default "/var/cache/hangar/git" $git.cache_dir }}
{{- if and .Values.cacheSizeLimit (hasPrefix "/var/cache/hangar/" $dir) }}
{{- $cache := include "hangar.bytes" (default "10GiB" $git.cache_size | toString) | float64 }}
{{- $volume := include "hangar.bytes" (toString .Values.cacheSizeLimit) | float64 }}
{{- if gt (mulf $cache 1.25) $volume }}
{{- fail (printf "cacheSizeLimit (%s) leaves too little room for git.cache_size (%s): the pack cache goes over its limit while Repos are in use, and the kubelet evicts the Pod when the volume fills. Make cacheSizeLimit at least a quarter larger than git.cache_size" (toString .Values.cacheSizeLimit) (default "10GiB" $git.cache_size | toString)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
A size in bytes, from a Kubernetes quantity (16Gi, 500M) or a Hangar size
(10GiB, 64MB, 1048576).
*/}}
{{- define "hangar.bytes" -}}
{{- $s := trim . }}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" $s }}
{{- if not $num }}
{{- fail (printf "invalid size %q" $s) }}
{{- end }}
{{- $unit := lower (trimPrefix $num $s) }}
{{- $units := dict "" 1.0 "b" 1.0 "k" 1000.0 "kb" 1000.0 "m" 1e6 "mb" 1e6 "g" 1e9 "gb" 1e9 "t" 1e12 "tb" 1e12 "ki" 1024.0 "kib" 1024.0 "mi" 1048576.0 "mib" 1048576.0 "gi" 1073741824.0 "gib" 1073741824.0 "ti" 1099511627776.0 "tib" 1099511627776.0 }}
{{- if not (hasKey $units $unit) }}
{{- fail (printf "invalid size %q: use a unit such as Mi, Gi, MiB or GiB" $s) }}
{{- end }}
{{- mulf (float64 $num) (get $units $unit) }}
{{- end }}

{{/*
Non-empty when templates can read the cluster with lookup. They cannot under
helm template, helm lint, --dry-run=client or Argo CD, where lookup returns
nothing. The default ServiceAccount exists in every namespace; on a first
install with --create-namespace the namespace does not exist yet, so the
default namespace stands in.
*/}}
{{- define "hangar.lookupWorks" -}}
{{- if lookup "v1" "ServiceAccount" .Release.Namespace "default" }}true
{{- else if lookup "v1" "Namespace" "" "default" }}true
{{- end }}
{{- end }}

{{/*
The checksum that restarts Pods when their secrets change: the `secrets`
values, the existingSecret name and, when the chart can read the cluster,
the existingSecret's data, so a rotated Secret rolls the Pods on the next
helm upgrade.
*/}}
{{- define "hangar.secretChecksum" -}}
{{- $existing := dict }}
{{- if .Values.existingSecret }}
{{- $s := lookup "v1" "Secret" .Release.Namespace .Values.existingSecret }}
{{- if $s }}
{{- $existing = default dict $s.data }}
{{- end }}
{{- end }}
{{- dict "secrets" .Values.secrets "existingSecret" .Values.existingSecret "existingData" $existing | toYaml | sha256sum }}
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
Non-empty when the generated Secret also holds the bootstrap admin password,
that is when the chart generates Secrets and `secrets` does not set the
password.
*/}}
{{- define "hangar.generatePassword" -}}
{{- if and (include "hangar.generate" .) (not (hasKey .Values.secrets "HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD")) }}true{{ end }}
{{- end }}

{{/* Non-empty when web and worker Pods run migrations in an init container. */}}
{{- define "hangar.migrateInit" -}}
{{- if and .Values.migrations.enabled (not .Values.persistence.enabled) (eq .Values.migrations.mode "initContainer") }}true{{ end }}
{{- end }}

{{/* Non-empty when a pre-install and pre-upgrade hook Job runs migrations. */}}
{{- define "hangar.migrateHook" -}}
{{- if and .Values.migrations.enabled (not .Values.persistence.enabled) (eq .Values.migrations.mode "hook") }}true{{ end }}
{{- end }}

{{/* env and envFrom of the Hangar container and the migrate init container. */}}
{{- define "hangar.env" -}}
env:
  - name: HANGAR_CONFIG
    value: /etc/hangar/hangar.yaml
  {{- with .Values.extraEnv }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
envFrom:
  {{- if or .Values.existingSecret .Values.secrets }}
  - secretRef:
      name: {{ include "hangar.secretName" . }}
  {{- end }}
  {{- if include "hangar.generate" . }}
  - secretRef:
      name: {{ include "hangar.fullname" . }}-generated
  {{- end }}
  {{- with .Values.extraEnvFrom }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
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
    checksum/secret: {{ include "hangar.secretChecksum" $ }}
    {{- with $v.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "hangar.serviceAccountName" $ }}
  automountServiceAccountToken: {{ $.Values.serviceAccount.automountServiceAccountToken }}
  {{- /*
  No service-link variables: with the release name hangar, the Service is
  named hangar and Kubernetes would inject HANGAR_SERVICE_HOST, HANGAR_PORT
  and the like, which Hangar rejects as unknown configuration.
  */}}
  enableServiceLinks: false
  {{- with $.Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    {{- toYaml $.Values.podSecurityContext | nindent 4 }}
  terminationGracePeriodSeconds: 45
  {{- if include "hangar.migrateInit" $ }}
  initContainers:
    # Brings the schema up to date before Hangar starts. It needs no Helm
    # hook, so GitOps tools such as Argo CD can apply the database in the
    # same sync: until the database answers, the container fails and the
    # kubelet retries it. Migrations take a lock in the database, so Pods
    # that start together wait for one another.
    - name: migrate
      image: {{ include "hangar.image" $ }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args: ["admin", "migrate"]
      {{- include "hangar.env" $ | nindent 6 }}
      resources:
        {{- toYaml $.Values.migrations.resources | nindent 8 }}
      securityContext:
        {{- toYaml $.Values.securityContext | nindent 8 }}
      volumeMounts:
        - {name: config, mountPath: /etc/hangar, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  {{- end }}
  containers:
    - name: hangar
      image: {{ include "hangar.image" $ }}
      imagePullPolicy: {{ $.Values.image.pullPolicy }}
      args: ["server", "--role", {{ .role | quote }}]
      {{- include "hangar.env" $ | nindent 6 }}
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
        # expected version and the Blob store answers. /readyz runs its checks
        # one after another and answers within 10 seconds
        # (httpx.ReadyTimeout), so wait longer than that: the default of
        # 1 second marks the pod unready whenever a healthy database or Blob
        # store is slow.
        httpGet: {path: /readyz, port: http}
        periodSeconds: 5
        timeoutSeconds: 15
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
