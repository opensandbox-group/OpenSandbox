---
title: Integrations
description: Ecosystem integrations for OpenSandbox — agent framework backends, protocol gateways, MCP servers, deployment platforms, and tooling, with maintenance status for each entry.
---

# Integrations

OpenSandbox connects to a growing ecosystem of agent frameworks, protocols, and platforms through independently maintained integrations. This page collects them in one place so they are easier to discover.

Each entry is labeled by maintenance status:

| Status | Meaning |
|--------|---------|
| Official | Developed and maintained in the `opensandbox-group` repositories |
| Community | Maintained by third-party maintainers outside the official repositories |
| Open PR | Contributed upstream to a framework's repository and under review; not yet merged |

::: tip
Runnable, in-repo walkthroughs — including coding agents, LangGraph, Deep Agents, Google ADK, and Microsoft Agent Framework — live under [Examples](/examples/). This page covers external ecosystem projects.
:::

## Agent Framework Backends and Providers

These projects let agent frameworks use OpenSandbox as the sandbox for code execution, file operations, and shell access.

| Project | Framework | Maintainer | Status |
|---------|-----------|------------|--------|
| [opensandbox-openai-agents](https://github.com/yjjnls/opensandbox-openai-agents) | OpenAI Agents SDK | [@yjjnls](https://github.com/yjjnls) | Community (early) |
| [opensandbox-deepagents](https://github.com/yjjnls/opensandbox-deepagents) | Deep Agents / Deep Agents Code | [@yjjnls](https://github.com/yjjnls) | Community (early) |
| [langchain-sandbox-opensandbox](https://github.com/AyushSonuu/langchain-sandbox-opensandbox) | Deep Agents (LangChain) | [@AyushSonuu](https://github.com/AyushSonuu) | Community |
| OpenSandboxDeployment backend ([SWE-agent/SWE-ReX#315](https://github.com/SWE-agent/SWE-ReX/pull/315)) | SWE-ReX (powers SWE-agent) | [@yjjnls](https://github.com/yjjnls) | Open PR |
| OpenSandbox sandbox backend ([rllm-org/rllm#976](https://github.com/rllm-org/rllm/pull/976)) | rllm | [@yjjnls](https://github.com/yjjnls) | Open PR |
| OpenSandbox cloud sandbox backend ([areal-project/AReaL#1955](https://github.com/areal-project/AReaL/pull/1955)) | AReaL | [@yjjnls](https://github.com/yjjnls) | Open PR |

Entries marked **Open PR** are contributions under review in the respective upstream projects. They are not part of those projects until the maintainers merge them, and their APIs may change during review. Entries marked **Community (early)** are new projects that may still have limited coverage or breaking changes.

## Protocol Compatibility

Gateways that expose OpenSandbox through other sandbox APIs.

| Project | Description | Maintainer | Status |
|---------|-------------|------------|--------|
| [E2BGateway](https://github.com/e2bgateway/e2bgateway) | Unified gateway that lets the official E2B client interoperate with OpenSandbox alongside other agent runtimes | e2bgateway | Community |

## MCP

The [OpenSandbox MCP Server](/sdks/mcp) is maintained in the main repository and exposes sandbox operations as MCP tools for Claude Code, Cursor, and other MCP-capable clients:

```bash
pip install opensandbox-mcp
```

Community MCP projects building on OpenSandbox:

| Project | Description | Maintainer | Status |
|---------|-------------|------------|--------|
| [demesne](https://github.com/jbeshir/demesne) | Local, containerized agent-orchestration MCP server that runs untrusted shell, scripts, and coding agents in disposable OpenSandbox containers (self-declared alpha) | [@jbeshir](https://github.com/jbeshir) | Community |

## Deployment Platforms

Community-maintained deployment automation for managed Kubernetes platforms. For self-managed Kubernetes, see the official [Deployment](/deployment/) documentation and the [AKS + Kata](/examples/aks-kata) and [ACK + PeerPods](/examples/ack-peerpods) examples.

| Project | Description | Maintainer | Status |
|---------|-------------|------------|--------|
| [opensandbox-on-eks](https://github.com/cloudthinker-ai/opensandbox-on-eks) | OpenSandbox platform for AI coding agents on Amazon EKS, with persistent environments | cloudthinker-ai | Community |
| [opensandbox-aks](https://github.com/weinong/opensandbox-aks) | OpenSandbox on Azure AKS | [@weinong](https://github.com/weinong) | Community |

## Images and Tooling

| Project | Description | Maintainer | Status |
|---------|-------------|------------|--------|
| [sandbox-runtime](https://github.com/tinkerfin-ai/sandbox-runtime) | Multi-architecture Linux sandbox image for OpenSandbox bundling common toolchains, headless Chromium, and a ready-to-use Python environment | TinkerFin | Community |
| [allox-cli](https://github.com/AlloxOrg/allox-cli) | CLI for resumable AI agent execution workspaces built on OpenSandbox | AlloxOrg | Community |

## Listing Your Integration

Maintaining an OpenSandbox integration? Pull requests are welcome to add a row to the relevant section. Please include the project name, a working link, the maintainer, and an accurate status label, and keep descriptions factual. See [Contributing](/community/contributing) for the general contribution process.
