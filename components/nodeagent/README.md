# OpenSandbox Node Agent

Node Agent runs once per Linux Kubernetes node. It merges one or more Sources
into a common pipeline, preserves order within each stream, and writes sandbox
records to a file or Alibaba Cloud OSS Sink. The stock binary ships the
`container-logs` Source for CRI stdout/stderr and the opt-in `syscalls` Source
for cgroup-scoped Linux system-call records.

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

The `syscalls` Source attaches one eBPF program to
`raw_syscalls/sys_enter`, filters events by sandbox-container cgroup, and emits
NDJSON. It requires Linux kernel 5.11 or newer, cgroup v2, tracefs, and the `BPF` and `PERFMON`
capabilities. The Helm chart adds these mounts and capabilities only when
`syscalls` is enabled. The Source persists active stream identity and outcome
metadata so a restart can reattach a live container or finalize a stream whose
Pod disappeared while the Agent was down. It does not persist eBPF event
payloads, so the restart interval remains an unobservable gap and is reported
as `syscall-agent-restart`. Its bounded Source data queue continues processing
lifecycle events under output backpressure and reports discarded events as
`syscall-source-backpressure`. It also attaches only after Kubernetes reports
the container ID, so its finalization marker is `incomplete` with
`syscall-attach-after-container-start` rather than claiming full-lifecycle
coverage. The filter tracks the runtime's container cgroup itself; processes
moved into delegated descendant cgroups are outside this first implementation.

The checked-in eBPF object is generated on Linux x86-64 with clang 18 and the
Ubuntu 24.04 libbpf headers. Run `make generate-syscalls-bpf CLANG=clang-18`
in this directory after changing the BPF C source.

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

See `manifests/charts/node-agent` for deployment settings. OSS
credentials must come from a Kubernetes Secret and must not include
`DeleteObject`; cleanup uses the separate offline command.

The file Sink currently cleans only complete `container-log` object families,
after the repair deadline and configured retention. Other record formats need
their own cleanup support. OSS cleanup remains an explicit offline operation.
Run `test/kind-smoke.sh` on a host with Docker, Kind, Helm, kubectl, and jq for
the restart and Pool-exclusion smoke test.
