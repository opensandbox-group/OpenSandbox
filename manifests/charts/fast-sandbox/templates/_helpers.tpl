{{/*
Expand the name of the chart.
*/}}
{{- define "fast-sandbox.name" -}}
{{- default "fast-sandbox" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "fast-sandbox.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "fast-sandbox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "fast-sandbox.labels" -}}
helm.sh/chart: {{ include "fast-sandbox.chart" . }}
app.kubernetes.io/name: {{ include "fast-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: fast-sandbox
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for the all-in-one controller (also selected by the
fast-sandbox-fastpath Service).
*/}}
{{- define "fast-sandbox.controllerSelectorLabels" -}}
app: fast-sandbox-controller
control-plane: controller-manager
fast-sandbox.io/control-plane-role: all
{{- end }}

{{/*
Component image reference. Call with a dict: repository, tag, appVersion. An
empty tag falls back to the release-<appVersion> companion image tag built
from the source pinned in manifests/third-party/fast-sandbox.commit.
*/}}
{{- define "fast-sandbox.image" -}}
{{- $tag := .tag | default (printf "release-%s" .appVersion) -}}
{{- printf "%s:%s" .repository $tag -}}
{{- end }}

{{/*
Namespace the workloads run in.
*/}}
{{- define "fast-sandbox.namespace" -}}
{{- default "opensandbox-system" .Values.systemNamespace }}
{{- end }}

{{/*
Whether DART P2P artifact delivery is enabled. Defaults to true when the
p2p map is absent from the values tree, so `helm upgrade --reuse-values`
from a pre-p2p-switch release (whose reused values carry no runtime.p2p
key and therefore skip the new chart default) keeps rendering the DART
resources instead of failing on a nil lookup.
*/}}
{{- define "fast-sandbox.p2pEnabled" -}}
{{- $p2p := dig "p2p" (dict) (.Values.runtime | default dict) -}}
{{- if hasKey $p2p "enabled" -}}{{ $p2p.enabled }}{{- else -}}true{{- end -}}
{{- end -}}

{{/*
Normalize controller.namespaces into a deduplicated, comma-separated string.
Empty output means cluster-wide mode.
*/}}
{{- define "fast-sandbox.watchedNamespacesCSV" -}}
{{- $normalized := list -}}
{{- range (.Values.controller.namespaces | default list) -}}
{{- $ns := trim (toString .) -}}
{{- if $ns -}}
{{- $normalized = append $normalized $ns -}}
{{- end -}}
{{- end -}}
{{- join "," (uniq $normalized) -}}
{{- end }}

{{/*
Shared RBAC rules for the fast-sandbox-controller Role / ClusterRole
(leader election leases live in the control-plane namespace Role).
*/}}
{{- define "fast-sandbox.controllerRules" -}}
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
- apiGroups:
  - ""
  resources:
  - configmaps
  verbs:
  - get
  - list
  - watch
  - create
- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
- apiGroups:
  - ""
  resources:
  - serviceaccounts
  verbs:
  - get
  - create
- apiGroups:
  - rbac.authorization.k8s.io
  resources:
  - roles
  - rolebindings
  verbs:
  - get
  - list
  - watch
  - create
  - update
- apiGroups:
  - ""
  resources:
  - events
  verbs:
  - create
  - patch
  - update
- apiGroups:
  - sandbox.fast.io
  resources:
  - sandboxes
  - sandboxpools
  - sandboxtemplates
  - sandboxsnapshots
  - sandboxes/status
  - sandboxpools/status
  - sandboxtemplates/status
  - sandboxsnapshots/status
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
{{- end }}

{{/*
Shared RBAC rules for the fast-sandbox-fastpath Role / ClusterRole.
*/}}
{{- define "fast-sandbox.fastpathRules" -}}
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - sandbox.fast.io
  resources:
  - sandboxpools
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - sandbox.fast.io
  resources:
  - sandboxes
  - sandboxes/status
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
- apiGroups:
  - sandbox.fast.io
  resources:
  - sandboxsnapshots
  - sandboxsnapshots/status
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
{{- end }}
