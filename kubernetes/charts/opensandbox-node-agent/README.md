# OpenSandbox Node Agent Helm Chart

This chart deploys one Node Agent per Linux node. It collects CRI stdout/stderr
from non-pooled OpenSandbox Pods and sends records to one configured file or
Alibaba Cloud OSS sink.

The chart is disabled by default when used through the umbrella OpenSandbox
chart. For OSS, create a Secret containing `access-key-id`,
`access-key-secret`, and optionally `session-token`, then set
`sink.type=oss` and `sink.oss.existingSecret`.

Node-local checkpoint state and source log retention jointly define the
delivery guarantee. Do not remove the state host path, rotate durable-file
outputs externally, or change a sink target without draining the Agent.
