{{/*
Helper templates for the NodeMedic on-call UI chart.

CRITICAL: `nodemedic-oncall-ui.requireTestCluster` enforces Constitution Article
I.5 (non-production clusters only). It is called from every other template
via the `include` mechanism so `helm template` / `helm install` aborts
unconditionally if `.Values.clusterName` is unset or doesn't match the
allowlist. This is the chart-level half of the belt-and-suspenders
cluster-name guard; the UI process also rejects on startup
(internal/oncall/server/cluster_guard.go — ValidateClusterName).

Allowlist (matches the spec §FR-23 surface):
  - cf1z (legacy CF Azure kubeadm)
  - jc1z (legacy CF Azure kubeadm)
  - sk1z (legacy CF Azure kubeadm)
  - test-* prefix (any AWS/EKS or Azure test cluster)
*/}}

{{- define "nodemedic-oncall-ui.requireTestCluster" -}}
{{- $name := .Values.clusterName -}}
{{- if not $name -}}
{{- fail "ERROR: .Values.clusterName is required (Constitution Article I.5 — non-production clusters only). Pass --set clusterName=cf1z|jc1z|sk1z (legacy CF dev clusters) or --set clusterName=test-<...> (test cluster)." -}}
{{- end -}}
{{- $allowed := list "cf1z" "jc1z" "sk1z" -}}
{{- if and (not (has $name $allowed)) (not (hasPrefix "test-" $name)) -}}
{{- fail (printf "ERROR: .Values.clusterName=%q must be one of cf1z/jc1z/sk1z or start with test- (Constitution Article I.5 — non-production clusters only)." $name) -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{- define "nodemedic-oncall-ui.fullname" -}}
{{- default "nodemedic-oncall-ui" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nodemedic-oncall-ui.labels" -}}
app.kubernetes.io/name: {{ include "nodemedic-oncall-ui.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: oncall-ui
nodemedic.cf.newrelic.com/cluster-name: {{ include "nodemedic-oncall-ui.requireTestCluster" . | quote }}
{{- end -}}

{{- define "nodemedic-oncall-ui.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nodemedic-oncall-ui.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nodemedic-oncall-ui.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
