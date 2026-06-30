---
title: Examples
description: Real-world usage examples for OpenSandbox covering coding agents, browser automation, desktop environments, and training workloads.
---

# Examples

OpenSandbox provides ready-to-run examples covering SDK usage, agent integrations, browser automation, and training workloads.

::: tip
All example source code is available in the [`examples/`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples) directory on GitHub.
:::

## Coding Agents

Run coding CLIs and AI agent frameworks inside isolated sandboxes.

| Example | Description |
|---------|-------------|
| [Claude Code](/examples/claude-code) | Run Claude Code CLI in a sandbox |
| [Gemini CLI](/examples/gemini-cli) | Run Gemini CLI in a sandbox |
| [Codex CLI](/examples/codex-cli) | Run OpenAI Codex CLI in a sandbox |
| [Qwen Code](/examples/qwen-code) | Run Qwen Code CLI in a sandbox |
| [Kimi CLI](/examples/kimi-cli) | Run Kimi CLI (Moonshot AI) in a sandbox |
| [LangGraph](/examples/langgraph) | LangGraph state-machine workflow with sandbox |
| [Google ADK](/examples/google-adk) | Google ADK agent using OpenSandbox tools |
| [OpenClaw](/examples/openclaw) | OpenClaw Gateway inside a sandbox |
| [NullClaw](/examples/nullclaw) | NullClaw Gateway sandbox integration |

## Browser & Desktop

Execute browser workloads and host desktop environments in sandboxes.

| Example | Description |
|---------|-------------|
| [Chrome](/examples/chrome) | Chromium sandbox with VNC and DevTools |
| [Playwright](/examples/playwright) | Playwright + Chromium headless testing |
| [Desktop](/examples/desktop) | Full desktop environment with VNC |
| [VS Code](/examples/vscode) | VS Code Web (code-server) in a sandbox |

## Core Usage

Fundamental sandbox operations and SDK workflows.

| Example | Description |
|---------|-------------|
| [Code Interpreter](/examples/code-interpreter) | End-to-end Code Interpreter SDK workflow |
| [AIO Sandbox](/examples/aio-sandbox) | All-in-One sandbox setup |
| [Agent Sandbox](/examples/agent-sandbox) | Kubernetes agent-sandbox integration |
| [AKS + Kata](/examples/aks-kata) | AKS deployment with Kata VM isolation, ingress, egress, and Credential Vault |
| [Windows](/examples/windows) | Windows sandbox via KVM/QEMU |
| [RL Training](/examples/rl-training) | DQN CartPole reinforcement learning |
| [Harbor Evaluation](/examples/harbor-evaluation) | Run a Harbor agent evaluation, one sandbox per trial |

## Storage

Persistent and shared storage patterns for sandboxes.

| Example | Description |
|---------|-------------|
| [Host Volume Mount](/examples/host-volume-mount) | Mount host directories into sandboxes |
| [Docker PVC Volume](/examples/docker-pvc-volume-mount) | Docker named volume mounts |
| [Docker OSSFS Volume](/examples/docker-ossfs-volume-mount) | Docker OSSFS (OSS FUSE) mounts |
| [Kubernetes PVC](/examples/kubernetes-pvc-volume-mount) | Kubernetes PersistentVolumeClaim mounts |

## How to Run

1. Start the OpenSandbox server (see [Quick Start](/getting-started/))
2. Set environment variables: `export SANDBOX_DOMAIN=...`, `export SANDBOX_API_KEY=...`
3. Add provider-specific variables as needed (e.g., `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`)
4. Navigate to the example directory and run: `python main.py`

::: tip
Each example includes a `main.py` entry point. Some also include a `Dockerfile` for containerized execution.
:::
