# OpenSandbox Node Agent

Node Agent runs once per Linux Kubernetes node. It merges one or more Sources
into a common pipeline, preserves order within each stream, and writes sandbox
records to a file or Alibaba Cloud OSS Sink. The stock binary currently ships
the `container-logs` Source for CRI stdout/stderr.

## Status

The component is experimental until the OSEP-0019 real-OSS fault suite and
published performance/24-hour soak report are complete. Do not infer a
production capacity from the chart's starting resource values.

## Development

```bash
make check
```

`NODEAGENT_SOURCES` is a comma-separated list of Sources compiled into the
binary and defaults to `container-logs`. Every Source owns its StreamRef
namespace and private state handle. Each emitted RecordKind has a registered
storage format that defines its encoding and object layout.

Every Source also receives an isolated view of the node-local sandbox Pod
store. It must call `Store.Forget` after it no longer needs a terminated Pod;
the Store keeps that identity until every enabled Source has released it.

For the current `container-log` format, the file Sink writes under:

```text
<cluster>/<namespace>/<sandbox_id>/<pod_uid>/<container>[.<generation>].log
```

The OSS sink uses the same suffix below `NODEAGENT_OSS_KEY_PREFIX`. Durable
progress is stored in `NODEAGENT_STATE_DIR/checkpoint.db`; it contains recovery
metadata, not record payloads. Do not remove it or change a target while streams
are active.

The process exposes `/healthz` and `/readyz` on
`NODEAGENT_SERVER_ADDR` (default `:8080`). Invalid configuration, state/target
identity conflicts, and unrecoverable sink results keep the process alive but
unready and stop progress.

See `kubernetes/charts/opensandbox-node-agent` for deployment settings. OSS
credentials must come from a Kubernetes Secret and must not include
`DeleteObject`; cleanup uses the separate offline command.

The file Sink currently cleans only complete `container-log` object families,
after the repair deadline and configured retention. Other record formats need
their own cleanup support. OSS cleanup remains an explicit offline operation.
Run `test/kind-smoke.sh` on a host with Docker, Kind, Helm, kubectl, and jq for
the restart and Pool-exclusion smoke test.
