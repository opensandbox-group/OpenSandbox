# OpenSandbox Roadmap

Last updated: 2026-11-25

This roadmap describes the intended project direction for roughly the next 12 months. It is a planning guide, not a release commitment. Implementation details are tracked through GitHub Issues, pull requests, and OpenSandbox Enhancement Proposals (OSEPs).

## Principles

- Keep public API contracts, SDKs, CLI behavior, implementation, examples, and documentation aligned.
- Prefer additive, backward-compatible changes for public interfaces.
- Use the OSEP process for major architecture, API, runtime, or security-model changes.
- Keep roadmap items focused on project direction instead of using this file as a full backlog.
- Move completed work into release notes, OSEP status, or stable documentation instead of keeping long historical task lists here.

## Current Focus: 2026 Q4 – 2027 H1

### Sandbox Runtime

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Persistent volumes | Implementing | [OSEP-0003](oseps/0003-volume-and-volumebinding-support.md) | Close remaining runtime/backend gaps beyond Docker and Kubernetes `host`/`pvc`/`ossfs` mounts, such as Fast Sandbox dynamic storage and additional volume backends. |
| Automated lifecycle management | Implementing | [OSEP-0024](oseps/0024-auto-pause-and-wake-on-access-for-fast-sandbox.md), [auto-pause guide](docs/guides/auto-pause-wake.md) | Auto-pause and wake-on-access are implemented for Fast Sandbox; extend the idle-pause and wake-on-access model to Pod-backed Docker and Kubernetes runtimes. |
| Fast Sandbox template and snapshot distribution | Implementing | [OSEP-0028](oseps/0028-rootfs-only-create-overrides.md), [OCI templates guide](docs/guides/oci-templates.md) | Build on Firecracker OverlayBD OCI templates ([OSEP-0007](oseps/0007-fast-sandbox-runtime-support.md)) and RootfsOnly snapshots with chunked transfer, locality-aware caching, per-create resource overrides, and broader runtime coverage. |
| Fast Sandbox ultra-high density and memory oversubscription | Implementing | [OSEP-0026](oseps/0026-resource-oversubscription-accounting-for-fast-sandbox.md), [resource-aware scheduling](docs/guides/resource-aware-scheduling.md), [PR #2915](https://github.com/opensandbox-group/OpenSandbox/pull/2915) | Extend the implemented overcommit accounting and virtio-balloon free-page reporting with working-set-aware admission, bounded active reclaim, and OOM-isolation guarantees. |
| Fast Sandbox cross-CPU-model memory snapshots and scheduling | Planned | TBD / OSEP needed | Build full-state memory-snapshot template variants per CPU microarchitecture and schedule restores onto matching Fastlet hosts across heterogeneous fleets (RootfsOnly snapshots already avoid CPU-model pinning for cold-boot restores). |
| In-sandbox privilege reduction and process QoS | Implementing | [OSEP-0018](oseps/0018-execd-as-sandbox-init.md), [OSEP-0031](oseps/0031-in-sandbox-process-qos-and-saturation-diagnostics.md), [privilege reduction guide](docs/guides/privilege-reduction.md) | Build on default-on `execd`-as-init hardening with server-managed `execd-ebpf` observation wiring, Fast Sandbox default hardening, and cgroup v2 process QoS isolation. |
| Isolated execution sessions | Implementing | [OSEP-0013](oseps/0013-isolated-execution-api.md), [isolation sessions guide](docs/guides/isolation-sessions.md) | Complete remaining phase 2 artifact diff and commit workflows on top of the implemented bubblewrap isolated-session runtime and SDK APIs. |

### Security and Egress

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Zero-trust egress and credential brokering | Implementing | [OSEP-0022](oseps/0022-multi-sandbox-egress-control-plane.md), [OSEP-0023](oseps/0023-credential-bound-tls-interception.md), [OSEP-0025](oseps/0025-credential-vault-provider-spi.md), [credential vault guide](docs/guides/credential-vault.md) | Mature multi-sandbox egress, credential-bound TLS interception rollout, dynamic credential exchange with caller authentication, and vault hardening. |
| Centralized egress gateway forwarding | Implementing | [OSEP-0030](oseps/0030-egress-upstream-proxy-forward-mode.md), [egress forward mode guide](docs/guides/egress-forward-mode.md) | Extend transparent fail-closed `CONNECT` forwarding (implemented for Docker and non-pooled Kubernetes) to pooled and Fast Sandbox workloads for external gateways such as agentgateway. |
| System-level control-plane authentication | Implementing | [OSEP-0032](oseps/0032-fastpath-caller-authentication.md), [Fast Sandbox system auth guide](docs/guides/fast-sandbox-system-auth.md) | Harden service-to-service boundaries across lifecycle server, ingress, and FastPath/Fastlet using Kubernetes-backed caller authentication over TLS. |
| Network isolation guidance | Ongoing | [network security guide](docs/guides/network-security.md) | Continue documenting safe defaults and practical isolation patterns. |

### SDKs and Developer Experience

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| SDK parity and MCP interface | Ongoing | [sdks/](sdks/), [specs/](specs/README.md), [OSEP-0033](oseps/0033-mcp-native-oauth-resource-server.md) | Keep Python, Go, Kotlin, JavaScript/TypeScript, C# SDKs, and `opensandbox-mcp` (including native OAuth 2.1 for Streamable HTTP) aligned with public specs. |
| CLI usability and extensibility | Ongoing | [cli/](cli/README.md) | Continue improving `osb` lifecycle, terminal, diagnostics, agent-skill, and plugin workflows. |
| Developer console and lifecycle history | Implementing | [console/](console/README.md), [developer console guide](docs/guides/developer-console.md), [OSEP-0029](oseps/0029-optional-postgresql-sandbox-lifecycle-history.md) | Mature the optional BFF + React console and add opt-in PostgreSQL lifecycle history for operational visibility. |
| Optional E2B ecosystem compatibility | Planned | [OSEP-0027](oseps/0027-optional-e2b-compatibility.md) | Evaluate an optional adapter/router shim (`components/e2b-shim`) enabling core E2B SDK agent workflows without altering native OpenSandbox contracts. |

### Observability and Operations

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Sandbox stdout/stderr collection | Implementing | [OSEP-0019](oseps/0019-node-agent-sandbox-collection.md), [node agent guide](docs/guides/node-agent.md) | Node-level container log, Firecracker `vm-console`, and optional syscall collection with Helm chart are implemented; complete the real-OSS fault suite and published 24-hour soak report before GA. |
| Agent in-sandbox audit trail | Planned | TBD / OSEP needed | Define auditable records for agent actions inside sandboxes—command/session execution, file operations, network access, identity context, retention, and privacy boundaries—building on [OSEP-0018](oseps/0018-execd-as-sandbox-init.md), [OSEP-0019](oseps/0019-node-agent-sandbox-collection.md), and [OSEP-0029](oseps/0029-optional-postgresql-sandbox-lifecycle-history.md). |
| Performance benchmarking and regression baselines | Implementing | [benchmark/](benchmark/README.md), [PR #3065](https://github.com/opensandbox-group/OpenSandbox/pull/3065) | Standardize repeatable in-cluster latency, throughput, density, and tracing benchmarks via `opensandbox-bench` with archived release baselines. |
| Kubernetes deployment | Ongoing | [kubernetes/](kubernetes/README.md), [Helm charts](manifests/charts/) | Keep self-hosted deployment, umbrella chart, and operational documentation current. |
| System-wide high availability | Planned | TBD / OSEP needed | Move beyond the current scoped HA (PostgreSQL-backed snapshot coordination and single-replica Fast Sandbox control plane) to full control-plane availability: multi-replica Lifecycle Server on Kubernetes, together with HA posture for ingress, node agent, and fast-sandbox components. |

### Public Contracts and Governance

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Lifecycle API stability | Ongoing | [specs/](specs/README.md), [OSEPs](oseps/README.md) | Preserve compatibility and require clear migration paths for user-visible changes across unified `1.x` umbrella releases ([OSEP-0016](oseps/0016-unified-umbrella-release-governance.md)). |
| Security documentation | Ongoing | [SECURITY.md](SECURITY.md), [docs/](docs/) | Keep vulnerability reporting, security expectations, and deployment guidance current. |
| Open project governance | Ongoing | [CHARTER.md](CHARTER.md), [GOVERNANCE.md](GOVERNANCE.md), [MAINTAINERS.md](MAINTAINERS.md), [CONTRIBUTING.md](CONTRIBUTING.md) | Operate transparently under the AAIF Charter and TSC process as the project and maintainer community grow. |

## Not Currently Planned

- Freezing the public API against additive evolution before cross-runtime lifecycle semantics and SDK parity are fully mature.
- Breaking public specs, SDK interfaces, CLI behavior, or documented workflows without an OSEP and migration path.
- Provider-specific features that cannot be documented, tested, or isolated cleanly from the public API surface.
- Fixed-calendar release trains beyond the lightweight unified umbrella release process ([OSEP-0016](oseps/0016-unified-umbrella-release-governance.md)) before maintainer capacity warrants it.

## How Roadmap Items Are Managed

- Small work is tracked as GitHub Issues and pull requests.
- Major features, architecture changes, public API changes, runtime behavior changes, and security-model changes start with an OSEP.
- Active roadmap entries should link to an issue, PR, OSEP, or documentation page once a stable tracking location exists.
- Completed work should be reflected in release notes, implemented OSEPs, and user documentation rather than remaining as an active roadmap item.
- Maintainers should review this file at least quarterly, or whenever a major OSEP changes status.
