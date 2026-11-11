# OpenSandbox Roadmap

Last updated: 2026-10-08

This roadmap describes the intended project direction for roughly the next 12 months. It is a planning guide, not a release commitment. Implementation details are tracked through GitHub Issues, pull requests, and OpenSandbox Enhancement Proposals (OSEPs).

## Principles

- Keep public API contracts, SDKs, CLI behavior, implementation, examples, and documentation aligned.
- Prefer additive, backward-compatible changes for public interfaces.
- Use the OSEP process for major architecture, API, runtime, or security-model changes.
- Keep roadmap items focused on project direction instead of using this file as a full backlog.
- Move completed work into release notes, OSEP status, or stable documentation instead of keeping long historical task lists here.

## Current Focus: 2026 H1-H2

### Sandbox Runtime

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Persistent volumes | Implementing | [OSEP-0003](oseps/0003-volume-and-volumebinding-support.md) | Close remaining runtime/backend gaps from OSEP-0003 before treating volume support as mature. |
| Secure container runtime | Implemented / maturing | [OSEP-0004](oseps/0004-secure-container-runtime.md), [secure container guide](docs/guides/secure-container.md) | Continue hardening isolation guidance and deployment practices. |
| Pause and resume via rootfs snapshot | Implemented / maturing | [OSEP-0008](oseps/0008-pause-resume-rootfs-snapshot.md) | Keep pause/resume behavior aligned across runtimes, SDKs, and docs for stateful sandbox workflows. |
| Automated lifecycle management | Planned | TBD / OSEP needed | Build on pause/resume ([OSEP-0008](oseps/0008-pause-resume-rootfs-snapshot.md)) and access-driven renewal ([OSEP-0009](oseps/0009-auto-renew-sandbox-on-ingress-access.md)): automatically pause idle sandboxes and resume them on demand, with the ingress gateway taking over traffic while a sandbox is suspended and waking it on first access. |
| Secure endpoint access | Implemented / maturing | [OSEP-0011](oseps/0011-secure-access-endpoint.md) | Keep endpoint security behavior aligned across server, SDKs, and docs. |
| Fast Sandbox template distribution | Implementing | [OSEP-0007](oseps/0007-fast-sandbox-runtime-support.md) | OverlayBD-based template packaging for large-scale, heterogeneous-runtime sandbox distribution (container / gVisor / Kata / Firecracker) through fast-sandbox template builds and on-demand restore. |
| Fast Sandbox ultra-high density and memory oversubscription | Planned | TBD / OSEP needed | Define the deployment and resource model for packing very high sandbox density per Fastlet Pod, including a memory oversubscription model with bounded overcommit, reclaim, and OOM-isolation guarantees. |
| Fast Sandbox cross-CPU-model templates and scheduling | Planned | TBD / OSEP needed | Build template variants per CPU model (vendor/generation microarchitecture) and schedule sandbox creates onto Fastlet nodes whose host CPU matches the template target, across heterogeneous hardware fleets. |

### SDKs and Developer Experience

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| SDK parity | Ongoing | [sdks/](sdks/), [specs/](specs/README.md) | Keep Python, Go, Kotlin, JavaScript/TypeScript, and C# SDKs aligned with public specs. |
| Client-side sandbox pool | Implemented / maturing | [OSEP-0005](oseps/0005-client-side-sandbox-pool.md) | Expand behavior consistency, tests, and documentation where practical. |
| CLI usability | Planned | [cli/](cli/README.md) | Improve common sandbox lifecycle workflows and developer ergonomics. |
| Developer console | Implementable | [OSEP-0006](oseps/0006-developer-console.md) | Provide a clearer operational surface for sandbox users and maintainers. |

### Observability and Operations

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| OpenTelemetry metrics and logs | Implementing | [OSEP-0010](oseps/0010-opentelemetry-instrumentation.md) | Add observability across execd, ingress, and egress. |
| Sandbox stdout/stderr collection | Implementing | [OSEP-0019](oseps/0019-node-agent-sandbox-collection.md) | Node agent collects sandbox stdout/stderr streams at node level and delivers them to backend storage with at-least-once guarantees. |
| eBPF in-sandbox observation | Implementing | [OSEP-0018](oseps/0018-execd-as-sandbox-init.md) | Optional eBPF observation of exec/connect/privilege events in the sandbox cgroup, emitted to a local rotating JSONL audit file (requires the `execd-ebpf` build + `CAP_BPF`). |
| Agent in-sandbox audit trail | Planned | TBD / OSEP needed | Define auditable records for agent actions inside sandboxes, such as command/session execution, file operations, network access, identity context, retention, and privacy boundaries. |
| Kubernetes deployment | Ongoing | [kubernetes/](kubernetes/README.md), [Helm charts](manifests/charts/) | Keep self-hosted deployment, chart, and operational documentation current. |
| Network isolation guidance | Ongoing | [network isolation guide](docs/guides/network-isolation.md) | Continue documenting safe defaults and practical isolation patterns. |
| System-wide high availability | Planned | TBD / OSEP needed | Move beyond the current scoped HA (PostgreSQL-backed snapshot coordination) to full control-plane availability: multi-replica Lifecycle Server on Kubernetes, together with HA posture for ingress, node agent, and fast-sandbox components. |

### Public Contracts and Governance

| Area | Status | Tracking | Notes |
|------|--------|----------|-------|
| Lifecycle API stability | Ongoing | [specs/](specs/README.md), [OSEPs](oseps/README.md) | Preserve compatibility and require clear migration paths for user-visible changes. |
| Security documentation | Ongoing | [SECURITY.md](SECURITY.md), [docs/](docs/) | Keep vulnerability reporting, security expectations, and deployment guidance current. |
| Open project governance | Ongoing | [CHARTER.md](CHARTER.md), [GOVERNANCE.md](GOVERNANCE.md), [CONTRIBUTING.md](CONTRIBUTING.md) | Maintain a lightweight, public decision process as the project grows. |

## Not Currently Planned

- Declaring a stable v1 API before lifecycle semantics, runtime behavior, and SDK compatibility are mature enough to support it.
- Breaking public specs, SDK interfaces, CLI behavior, or documented workflows without an OSEP and migration path.
- Provider-specific features that cannot be documented, tested, or isolated cleanly from the public API surface.
- Heavy release-train governance before the project has enough maintainer capacity to keep that process current.

## How Roadmap Items Are Managed

- Small work is tracked as GitHub Issues and pull requests.
- Major features, architecture changes, public API changes, runtime behavior changes, and security-model changes start with an OSEP.
- Active roadmap entries should link to an issue, PR, OSEP, or documentation page once a stable tracking location exists.
- Completed work should be reflected in release notes, implemented OSEPs, and user documentation rather than remaining as an active roadmap item.
- Maintainers should review this file at least quarterly, or whenever a major OSEP changes status.
