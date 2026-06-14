{{/*
Helper templates for the NodeMedic agent chart.

CRITICAL: `nodemedic-agent.requireTestCluster` enforces Constitution Article
I.5 (non-production clusters only). It is called from every other template
via the `include` mechanism so `helm template` / `helm install` aborts
unconditionally if `.Values.clusterName` is unset or doesn't match the
allowlist. This is the chart-level half of the belt-and-suspenders
cluster-name guard; the agent process also rejects on startup
(nodemedic_agent/config.py — Settings._validate_cluster_allowlist).

Allowlist (matches the spec §G10 surface):
  - cf1z (legacy CF Azure kubeadm)
  - jc1z (legacy CF Azure kubeadm)
  - sk1z (legacy CF Azure kubeadm)
  - test-* prefix (any AWS/EKS or Azure test cluster)
*/}}

{{- define "nodemedic-agent.requireTestCluster" -}}
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

{{- define "nodemedic-agent.fullname" -}}
{{- default "nodemedic-agent" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nodemedic-agent.labels" -}}
app.kubernetes.io/name: {{ include "nodemedic-agent.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: agent
nodemedic.cf.newrelic.com/cluster-name: {{ include "nodemedic-agent.requireTestCluster" . | quote }}
{{- end -}}

{{- define "nodemedic-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nodemedic-agent.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nodemedic-agent.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
