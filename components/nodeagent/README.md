# OpenSandbox Node Agent

Node Agent runs once per Linux Kubernetes node and collects CRI stdout/stderr
from non-pooled OpenSandbox Pods. Records are enriched with sandbox and
Kubernetes identity and written to one durable file or Alibaba Cloud OSS sink.

## Status

The component is experimental until the OSEP-0019 real-OSS fault suite and
published performance/24-hour soak report are complete. Do not infer a
production capacity from the chart's starting resource values.

## Development

```bash
make check
```

The file sink writes under:

```text
<cluster>/<namespace>/<sandbox_id>/<pod_uid>/<container>[.<generation>].log
```

The OSS sink uses the same suffix below `NODEAGENT_OSS_KEY_PREFIX`. Durable
progress is stored in `NODEAGENT_STATE_DIR/checkpoint.db`; it contains recovery
metadata, not log payloads. Do not remove it or change a target while streams
are active.

The process exposes `/healthz` and `/readyz` on
`NODEAGENT_SERVER_ADDR` (default `:8080`). Invalid configuration, state/target
identity conflicts, and unrecoverable sink results keep the process alive but
unready and stop progress.

See `kubernetes/charts/opensandbox-node-agent` for deployment settings. OSS
credentials must come from a Kubernetes Secret and must not include
`DeleteObject`; cleanup uses the separate offline command.

The file Sink cleans only complete object families, after the repair deadline
and configured retention. OSS cleanup remains an explicit offline operation.
Run `test/kind-smoke.sh` on a host with Docker, Kind, Helm, kubectl, and jq for
the restart and Pool-exclusion smoke test.
