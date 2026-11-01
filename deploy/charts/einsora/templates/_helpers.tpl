{{- define "einsora.labels" -}}
app.kubernetes.io/part-of: einsora
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "einsora.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end }}

{{- /* Environment shared by every control plane process. */}}
{{- define "einsora.env" -}}
- name: EINSORA_ENV
  value: {{ .Values.env | quote }}
- name: EINSORA_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: DATABASE_URL
- name: EINSORA_AUTH_MODE
  value: oidc
- name: EINSORA_OIDC_ISSUER
  value: {{ .Values.auth.oidcIssuer | quote }}
- name: EINSORA_OIDC_AUDIENCE
  value: {{ .Values.auth.oidcAudience | quote }}
- name: EINSORA_OIDC_TENANT_CLAIM
  value: {{ .Values.auth.tenantClaim | quote }}
- name: EINSORA_CLUSTER_BACKEND
  value: kubernetes
- name: EINSORA_UNTRUSTED_RUNTIME_CLASS
  value: {{ .Values.cluster.untrustedRuntimeClass | quote }}
- name: EINSORA_STORAGE_CLASS
  value: {{ .Values.cluster.storageClass | quote }}
- name: EINSORA_IMAGES_NAMESPACE
  value: {{ .Values.cluster.imagesNamespace | quote }}
{{- end }}

{{- define "einsora.podSecurityContext" -}}
securityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
{{- end }}

{{- define "einsora.containerSecurityContext" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
{{- end }}

{{- define "einsora.catalogVolume" -}}
volumes:
  - name: catalog
    configMap:
      name: einsora-catalog
{{- end }}

{{- define "einsora.catalogMount" -}}
volumeMounts:
  - name: catalog
    mountPath: /etc/einsora
    readOnly: true
{{- end }}

{{/*
The region this cluster's controller reconciles (ADR-0009). It must be a region ID and be listed in
the catalog, or rendering fails: a controller for a region nobody can create workloads in would leave
every workload pending without a word.
*/}}
{{- define "einsora.region" -}}
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
