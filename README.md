<div align="center">
  <img src="docs/public/images/logo.svg" alt="OpenSandbox logo" width="150" />

  <h1>OpenSandbox</h1>

  <p align="center">
    <a href="https://trendshift.io/repositories/21828" target="_blank"><img src="https://trendshift.io/api/badge/repositories/21828" alt="opensandbox-group%2FOpenSandbox | Trendshift" style="width: 320px; height: 70px;" width="320" height="70" /></a>
  </p>

<p align="center">
  <a href="https://github.com/opensandbox-group/OpenSandbox"><img src="https://img.shields.io/github/stars/opensandbox-group/OpenSandbox?style=flat-square&logo=github&logoColor=white&label=Stars&color=181717" alt="Stars" /></a>
  <a href="https://www.bestpractices.dev/projects/12588"><img src="https://img.shields.io/badge/OpenSSF-Best-4C566A?style=flat-square" alt="OpenSSF Best Practices" /></a>
  <a href="https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox"><img src="https://img.shields.io/badge/CNCF-Landscape-0C66E4?style=flat-square" alt="CNCF Landscape" /></a>
  <a href="https://discord.gg/g7FuPs8YeD"><img src="https://img.shields.io/badge/Discord-Join-5865F2?style=flat-square&logo=discord&logoColor=white" alt="Discord" /></a>
  <a href="https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11"><img src="https://img.shields.io/badge/DingTalk-Join-0089FF?style=flat-square" alt="DingTalk" /></a>
  <a href="https://github.com/opensandbox-group/OpenSandbox/actions"><img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/real-e2e.yml?branch=main&label=TEST&style=flat-square&logo=github&logoColor=white" alt="E2E Status" /></a>
  <a href="https://github.com/opensandbox-group/OpenSandbox/actions"><img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/kubernetes-nightly-build.yml?branch=main&label=K8S&style=flat-square&logo=kubernetes&logoColor=white" alt="Kubernetes nightly build status" /></a>
</p>

  <hr />
</div>

OpenSandbox is a **general-purpose sandbox platform** for AI applications. It gives Coding Agents, GUI Agents, Agent Evaluation, AI Code Execution, and RL Training workloads a secure, scalable place to run — with the same API from a laptop to a large cluster.

## Features

- 🧩 **SDKs, CLI, and MCP**: Native SDKs for Python, Java/Kotlin, TypeScript, C#/.NET, and Go, plus the `osb` CLI and an MCP server — one API surface for sandbox creation, command execution, and file operations. See [SDKs](#sdks), [CLI](#cli), and [MCP](#mcp).
- 📜 **Open Protocol**: Sandbox lifecycle and execution APIs are defined as public OpenAPI contracts, so custom runtimes can plug in without changing client code. See [API specs](specs/README.md).
- 🚀 **Sandbox Runtime and Environments**: Docker and Kubernetes runtimes behind the same SDK calls, with built-in Command, Filesystem, and Code Interpreter environments — covering Coding Agents (e.g., Claude Code), browser automation (Chrome, Playwright), and desktop environments (VNC, VS Code). See [Kubernetes runtime](./kubernetes).
- ⚡ **Hybrid Deployment**: Mix long-running, Kubernetes-native container workloads with short-lived microVM sandboxes in one cluster. Pre-warmed, Firecracker-backed pools give constant-time admission and ~80ms startup; FastSandbox pause/resume checkpoints state to the artifact store and releases all compute, resuming on any host. See [Fast Sandbox](docs/architecture/fast-sandbox/scheduling.md).
- 🚦 **Network Policy and Credential Vault**: Unified ingress gateway with multiple routing strategies, per-sandbox egress controls, and secure credential injection that keeps real secrets away from sandbox workloads. See [Ingress Gateway](components/ingress), [egress controls](components/egress), and [Credential Vault](docs/guides/credential-vault.md).
- 🏰 **Strong Isolation**: Run workloads under gVisor, Kata Containers, or Firecracker microVMs for strong isolation from the host. See the [Secure Container Runtime Guide](docs/guides/secure-container.md).

## Official Container Images

OpenSandbox release images are published under the same component name in
three official registries:

- Docker Hub: `docker.io/opensandbox/<component>`
- GitHub Container Registry: `ghcr.io/opensandbox-group/opensandbox/<component>`
- Alibaba Cloud Container Registry: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/<component>`

Tagged release images are signed keylessly with Cosign and include provenance
attestations. Pin production images by digest and follow the
[release verification guide](docs/community/release-verification.md) to verify
the image against the OpenSandbox GitHub Actions identity before deployment.

## SDKs

Pick your language:

<details>
<summary><b>Python</b></summary>

```bash
pip install opensandbox
```

</details>

<details>
<summary><b>Java/Kotlin (Gradle Kotlin DSL)</b></summary>

```kotlin
dependencies {
    implementation("com.alibaba.opensandbox:sandbox:{latest_version}")
}
```

</details>

<details>
<summary><b>Java/Kotlin (Maven)</b></summary>

```xml
<dependency>
    <groupId>com.alibaba.opensandbox</groupId>
    <artifactId>sandbox</artifactId>
    <version>{latest_version}</version>
</dependency>
```

</details>

<details>
<summary><b>JavaScript/TypeScript</b></summary>

```bash
npm install @alibaba-group/opensandbox
```

</details>

<details>
<summary><b>C#/.NET</b></summary>

```bash
dotnet add package Alibaba.OpenSandbox
```

</details>

<details>
<summary><b>Go</b></summary>

```bash
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

</details>

## CLI

OpenSandbox also provides `osb`, a terminal CLI for the common sandbox workflow: create sandboxes, run commands, move files, inspect diagnostics, and manage runtime egress policy.

Install:

```bash
pip install opensandbox-cli
# or
uv tool install opensandbox-cli
```

Quick start:

```bash
osb config init
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb config set connection.api_key <your-api-key>
osb sandbox create --image python:3.12 --timeout 30m -o json
osb command run <sandbox-id> -o raw -- python -c "print(1 + 1)"
```

See the [CLI README](cli/README.md) for the full command reference.

## MCP

The OpenSandbox MCP server exposes sandbox creation, command execution, and text file operations to MCP-capable clients such as Claude Code and Cursor.

Install and run:

```bash
pip install opensandbox-mcp
opensandbox-mcp --domain localhost:8080 --protocol http
```

Minimal stdio config:

```json
{
  "mcpServers": {
    "opensandbox": {
      "command": "opensandbox-mcp",
      "args": ["--domain", "localhost:8080", "--protocol", "http"]
    }
  }
}
```

See the [MCP README](sdks/mcp/sandbox/python/README.md) for client-specific setup.

## Getting Started

Requirements:

- Docker (required for local execution)
- Python 3.10+ (required for examples and local runtime)

### Install and Configure the Sandbox Server

```bash
uvx opensandbox-server init-config ~/.sandbox.toml --example docker

uvx opensandbox-server

# Show help
# uvx opensandbox-server -h
```

### Create a Sandbox and Execute Commands/Scripts

Install the Sandbox SDK

```bash
uv pip install opensandbox
```

Create a sandbox from an `alpine` image and execute commands and scripts.

```python
import asyncio

from opensandbox import Sandbox
from opensandbox.models import WriteEntry

async def main() -> None:
    # 1. Create a sandbox from the alpine image
    sandbox = await Sandbox.create("alpine")

    try:
        # 2. Execute a shell command
        execution = await sandbox.commands.run("echo 'Hello OpenSandbox!'")
        print(execution.logs.stdout[0].text)

        # 3. Write a script file
        await sandbox.files.write_files([
            WriteEntry(
                path="/tmp/hello.sh",
                data="echo \"Hello $1\"\necho '2 + 2 =' $((2 + 2))",
                mode=755,
            )
        ])

        # 4. Read the file back
        content = await sandbox.files.read_file("/tmp/hello.sh")
        print(f"Content: {content}")

        # 5. Execute the script
        execution = await sandbox.commands.run("sh /tmp/hello.sh OpenSandbox")
        for log in execution.logs.stdout:
            print(log.text)

    finally:
        # 6. Cleanup the sandbox
        await sandbox.destroy()

if __name__ == "__main__":
    asyncio.run(main())
```

### More Examples

OpenSandbox provides examples covering SDK usage, agent integrations, browser automation, and training workloads. All example code is located in the `examples/` directory.

#### 🎯 Basic Examples

- **[code-interpreter](docs/examples/code-interpreter.md)** - End-to-end Code Interpreter SDK workflow in a sandbox.
- **[aio-sandbox](docs/examples/aio-sandbox.md)** - All-in-One sandbox setup using the OpenSandbox SDK.
- **[agent-sandbox](docs/examples/agent-sandbox.md)** - Example integration for running OpenSandbox workloads on Kubernetes with [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).
- **Volumes** — [Docker PVC / named volumes](docs/examples/docker-pvc-volume-mount.md), [Docker OSSFS](docs/examples/docker-ossfs-volume-mount.md), [Kubernetes PVC](docs/examples/kubernetes-pvc-volume-mount.md): persistent and shared storage patterns.

#### 🤖 Coding Agent Integrations

- **Coding CLIs** — [Claude Code](docs/examples/claude-code.md), [Gemini CLI](docs/examples/gemini-cli.md), [OpenAI Codex CLI](docs/examples/codex-cli.md), [OpenCode](docs/examples/opencode.md), [Qwen Code](docs/examples/qwen-code.md), [Kimi CLI](docs/examples/kimi-cli.md): run each CLI inside OpenSandbox.
- **[langgraph](docs/examples/langgraph.md)** - LangGraph state-machine workflow that creates/runs a sandbox job with fallback retry.
- **[google-adk](docs/examples/google-adk.md)** - Google ADK agent using OpenSandbox tools to write/read files and run commands.
- **[openclaw](docs/examples/openclaw.md)** - Launch an OpenClaw Gateway inside a sandbox.
- **[deer-flow](docs/examples/deer-flow.md)** - DeerFlow agent turns whose shell, file, and search tools run inside a sandbox through its built-in OpenSandbox provider.

#### 🌐 Browser and Desktop Environments

- **[chrome](docs/examples/chrome.md)** - Chromium sandbox with VNC and DevTools access for automation and debugging.
- **[playwright](docs/examples/playwright.md)** - Playwright + Chromium headless scraping and testing example.
- **[desktop](docs/examples/desktop.md)** - Full desktop environment in a sandbox with VNC access.
- **[vscode](docs/examples/vscode.md)** - code-server (VS Code Web) running inside a sandbox for remote dev.

#### 🧠 Training and Evaluation

- **[harbor-evaluation](docs/examples/harbor-evaluation.md)** - Run a [Harbor](https://github.com/harbor-framework/harbor) agent evaluation on OpenSandbox, one sandbox per trial.

For more details, please refer to the [examples documentation](docs/examples/index.md).

## Documentation

- [Architecture](docs/architecture/) – Overall architecture & design philosophy
- [Credential Vault](docs/guides/credential-vault.md) - Credential Vault credential injection guide
- [Release Verification](docs/community/release-verification.md) - Release signing and artifact verification
- [oseps/README.md](oseps/README.md) – OpenSandbox Enhancement Proposals
- SDK - sandbox lifecycle, command execution, and file operations
  - [Java/Kotlin](sdks/sandbox/kotlin/README.md)
  - [Python](sdks/sandbox/python/README.md)
  - [JavaScript/TypeScript](sdks/sandbox/javascript/README.md)
  - [C#/.NET](sdks/sandbox/csharp/README.md)
  - [Go](sdks/sandbox/go/README.md)
- [cli/README.md](cli/README.md) - OpenSandbox CLI installation and command reference
- [sdks/mcp/sandbox/python/README.md](sdks/mcp/sandbox/python/README.md) - MCP server installation and client setup
- [specs/README.md](specs/README.md) - OpenAPI definitions for sandbox lifecycle API and sandbox execution API
- [server/README.md](server/README.md) - Sandbox server startup and configuration; supports Docker and Kubernetes runtimes
- [ROADMAP.md](ROADMAP.md) - Lightweight project roadmap and planning process

## License

This project is open source under the [Apache 2.0 License](LICENSE).

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the current project roadmap, planning scope,
and how roadmap items are managed.

## Contact and Discussion

- Issues: Submit bugs, feature requests, or design discussions through GitHub Issues
- Discord: Join the [OpenSandbox Discord community](https://discord.gg/g7FuPs8YeD)
- DingTalk: Join the [OpenSandbox technical discussion group](https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11)
