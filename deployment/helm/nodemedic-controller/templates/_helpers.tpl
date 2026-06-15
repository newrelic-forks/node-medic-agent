{{/*
Helper templates for the NodeMedic controller chart.

CRITICAL: `nodemedic.requireTestCluster` enforces Constitution Article I.9
(test clusters only). It is called from every other template via the
`include` mechanism so that `helm template` / `helm install` aborts
unconditionally if `.Values.clusterName` does not start with `test-`.
This is the chart-level half of the belt-and-suspenders cluster-name
guard; the controller binary also rejects on startup (cmd/main.go
validateClusterName).
*/}}

{{- define "nodemedic.requireTestCluster" -}}
{{- $name := .Values.clusterName -}}
{{- if not $name -}}
{{- fail "ERROR: .Values.clusterName is required (Constitution Article I.9 — test clusters only). Pass --set clusterName=cf1z (Azure kubeadm) or --set clusterName=test-<...> (AWS/EKS test cluster)." -}}
{{- end -}}
{{- if and (ne $name "cf1z") (not (hasPrefix "test-" $name)) -}}
{{- fail (printf "ERROR: .Values.clusterName=%q must be either `cf1z` (Azure kubeadm) or start with `test-` (AWS/EKS test clusters) (Constitution Article I.9 — test clusters only)." $name) -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{- define "nodemedic.fullname" -}}
{{- default "nodemedic-controller" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nodemedic.labels" -}}
app.kubernetes.io/name: {{ include "nodemedic.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: controller
nodemedic.cf.newrelic.com/cluster-name: {{ include "nodemedic.requireTestCluster" . | quote }}
{{- end -}}

{{- define "nodemedic.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nodemedic.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nodemedic.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
