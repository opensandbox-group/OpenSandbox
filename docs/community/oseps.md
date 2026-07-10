---
title: Enhancement Proposals (OSEPs)
description: Index of OpenSandbox Enhancement Proposals covering major features, architectural changes, and API modifications.
---

# OpenSandbox Enhancement Proposals

OpenSandbox Enhancement Proposals (OSEPs) are the mechanism for proposing major features, architectural changes, or modifications to the core API and security model. Small bug fixes and minor improvements do not require an OSEP.

See the [OSEP contributing guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/CONTRIBUTING.md) for information on how to create and merge proposals.

## Proposal Index

| OSEP | Title | Status | Last Updated |
|:----:|:-----:|:------:|:------------:|
| [OSEP-0001](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0001-fqdn-based-egress-control.md) | FQDN-based Egress Control | implemented | 2026-01-22 |
| [OSEP-0002](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0002-kubernetes-sigs-agent-sandbox-support.md) | kubernetes-sigs/agent-sandbox Support | implemented | 2026-01-23 |
| [OSEP-0003](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0003-volume-and-volumebinding-support.md) | Volume Support | implementing | 2026-02-11 |
| [OSEP-0004](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0004-secure-container-runtime.md) | Pluggable Secure Container Runtime Support | implemented | 2026-02-09 |
| [OSEP-0005](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0005-client-side-sandbox-pool.md) | Client-Side Sandbox Pool | implementing | 2026-03-09 |
| [OSEP-0006](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0006-developer-console.md) | Developer Console for Sandbox Operations | implementable | 2026-03-06 |
| [OSEP-0007](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0007-fast-sandbox-runtime-support.md) | Fast Sandbox Runtime Support | provisional | 2026-02-08 |
| [OSEP-0008](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0008-pause-resume-rootfs-snapshot.md) | Pause and Resume via Rootfs Snapshot | implementing | 2026-03-13 |
| [OSEP-0009](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md) | Auto-Renew Sandbox on Ingress Access | implemented | 2026-03-23 |
| [OSEP-0010](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0010-opentelemetry-instrumentation.md) | OpenTelemetry Metrics and Logs (execd, egress, and ingress) | implementing | 2026-04-12 |
| [OSEP-0011](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0011-secure-access-endpoint.md) | Secure Access on GetEndpoint and Signed Endpoint | implemented | 2026-04-25 |
| [OSEP-0012](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0012-credential-vault.md) | Credential Vault and Credential Proxy | implemented | 2026-06-23 |
| [OSEP-0013](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0013-isolated-execution-api.md) | Isolated Execution API | implementing | 2026-06-23 |
| [OSEP-0014](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0014-multi-tenancy.md) | Multi-Tenancy Support for Kubernetes Runtime | draft | 2026-04-29 |

## Status Definitions

| Status | Meaning |
|--------|---------|
| **draft** | Proposal is under initial development, not yet accepted |
| **provisional** | Proposal accepted in principle, design may still evolve |
| **implementable** | Design is final and ready for implementation |
| **implementing** | Implementation is in progress |
| **implemented** | Fully implemented and available |

## Submitting a Proposal

::: tip
Before writing a full OSEP, consider opening a GitHub Issue or Discussion to gauge community interest and get early feedback on your idea.
:::

To submit a new OSEP:

1. Fork the repository
2. Copy the OSEP template
3. Fill in the proposal details
4. Submit a pull request

See the [OSEP contributing guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/CONTRIBUTING.md) for the full process.
