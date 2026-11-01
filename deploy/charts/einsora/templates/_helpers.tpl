{{- define "tensorafi.labels" -}}
app.kubernetes.io/part-of: tensorafi
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "tensorafi.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end }}

{{- /* Environment shared by every control plane process. */}}
{{- define "tensorafi.env" -}}
- name: TENSORAFI_ENV
  value: {{ .Values.env | quote }}
- name: TENSORAFI_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: DATABASE_URL
- name: TENSORAFI_AUTH_MODE
  value: oidc
- name: TENSORAFI_OIDC_ISSUER
  value: {{ .Values.auth.oidcIssuer | quote }}
- name: TENSORAFI_OIDC_AUDIENCE
  value: {{ .Values.auth.oidcAudience | quote }}
- name: TENSORAFI_OIDC_TENANT_CLAIM
  value: {{ .Values.auth.tenantClaim | quote }}
- name: TENSORAFI_CLUSTER_BACKEND
  value: kubernetes
- name: TENSORAFI_UNTRUSTED_RUNTIME_CLASS
  value: {{ .Values.cluster.untrustedRuntimeClass | quote }}
- name: TENSORAFI_STORAGE_CLASS
  value: {{ .Values.cluster.storageClass | quote }}
- name: TENSORAFI_IMAGES_NAMESPACE
  value: {{ .Values.cluster.imagesNamespace | quote }}
{{- end }}

{{- define "tensorafi.podSecurityContext" -}}
securityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
{{- end }}

{{- define "tensorafi.containerSecurityContext" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
{{- end }}

{{- define "tensorafi.catalogVolume" -}}
volumes:
  - name: catalog
    configMap:
      name: tensorafi-catalog
{{- end }}

{{- define "tensorafi.catalogMount" -}}
volumeMounts:
  - name: catalog
    mountPath: /etc/tensorafi
    readOnly: true
{{- end }}

{{/*
The region this cluster's controller reconciles (ADR-0009). It must be a region ID and be listed in
the catalog, or rendering fails: a controller for a region nobody can create workloads in would leave
every workload pending without a word.
*/}}
{{- define "tensorafi.region" -}}
{{- $region := required "cluster.region is required (ADR-0009)" .Values.cluster.region -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$" $region) -}}
{{- fail (printf "cluster.region %q isn't a region ID (lowercase letters, digits, and '-')" $region) -}}
{{- end -}}
{{- $ids := list -}}
{{- range (fromYaml .Values.catalog).regions -}}
{{- $ids = append $ids .id -}}
{{- end -}}
{{- if not (has $region $ids) -}}
{{- fail (printf "cluster.region %q isn't in the catalog's regions %v" $region $ids) -}}
{{- end -}}
{{- $region -}}
{{- end }}
