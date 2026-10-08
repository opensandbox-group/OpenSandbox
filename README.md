<div align="center">
  <img src="docs/public/images/logo.svg" alt="OpenSandbox logo" width="150" />

  <h1>OpenSandbox</h1>

<p align="center">
  <a href="https://github.com/opensandbox-group/OpenSandbox"><img src="https://img.shields.io/github/stars/opensandbox-group/OpenSandbox?style=flat-square&logo=github&logoColor=white&label=Stars&color=181717" alt="Stars" /></a>
  <a href="https://www.bestpractices.dev/projects/12588"><img src="https://img.shields.io/badge/OpenSSF-Best-4C566A?style=flat-square" alt="OpenSSF Best Practices" /></a>
  <a href="https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox"><img src="https://img.shields.io/badge/CNCF-Landscape-0C66E4?style=flat-square" alt="CNCF Landscape" /></a>
  <a href="https://discord.gg/g7FuPs8YeD"><img src="https://img.shields.io/badge/Discord-Join-5865F2?style=flat-square&logo=discord&logoColor=white" alt="Discord" /></a>
  <a href="https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11"><img src="https://img.shields.io/badge/DingTalk-Join-0089FF?style=flat-square" alt="DingTalk" /></a>
  <a href="https://github.com/opensandbox-group/OpenSandbox/actions"><img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/real-e2e.yml?branch=main&label=TEST&style=flat-square&logo=github&logoColor=white" alt="E2E Status" /></a>
  <a href="https://github.com/opensandbox-group/OpenSandbox/actions"><img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/kubernetes-nightly-build.yml?branch=main&label=K8S&style=flat-square&logo=kubernetes&logoColor=white" alt="Kubernetes nightly build status" /></a>
</p>

  <p align="center">
    <a href="https://trendshift.io/repositories/21828" target="_blank"><img src="https://trendshift.io/api/badge/repositories/21828" alt="opensandbox-group%2FOpenSandbox | Trendshift" style="width: 320px; height: 70px;" width="320" height="70" /></a>
  </p>

  <hr />
</div>

**Run AI agents in sandboxes on your own infrastructure.**

OpenSandbox gives AI applications isolated environments to execute code, run commands, manage files, and operate browsers or desktops. Start locally with Docker and deploy on Kubernetes through a unified sandbox API.

[Quick Start](#getting-started) · [Examples](#examples) · [Documentation](#documentation) · [Fast Sandbox](docs/architecture/fast-sandbox/index.md)

## Features

| Feature | What it enables | Learn more |
|---------|-----------------|------------|
| **Fast Sandbox runtime** | Fast, high-density sandboxes on Kubernetes. Firecracker creation: **64 ms P50 (serial)** / **125.4 ms P99 (10 concurrent)**. Firecracker sandboxes support pause/resume with memory and disk state preserved. | [Integration](docs/architecture/fast-sandbox/index.md) · [Performance](docs/architecture/fast-sandbox/performance.md) · [Pause/resume](docs/architecture/fast-sandbox/checkpoints.md) |
| **Agent working environments** | Execute commands, manage files, and run code with built-in APIs. Integration examples show how to run coding agents, browsers, and desktops inside sandboxes. | [Examples](docs/examples/index.md) |
| **Network access control** | Route inbound traffic through a unified ingress gateway and control outbound access with per-sandbox egress policies. | [Ingress](docs/architecture/network/ingress.md) · [Egress](docs/architecture/network/egress.md) |
| **Credential Vault** | Let agents call external services without exposing real credentials to sandbox workloads. | [Credential Vault](docs/guides/credential-vault.md) |
| **Local to cluster** | Start with Docker and deploy on Kubernetes through a unified lifecycle API. Resource pools and batch creation support agent evaluation and RL training workloads. | [Kubernetes runtime](docs/architecture/control-plane/operator.md) |
| **SDKs, CLI, and MCP** | Integrate with Python, Java/Kotlin, TypeScript/JavaScript, C#/.NET, or Go SDKs. Use `osb` from the terminal or connect agents through MCP. | [SDKs](#sdks) · [CLI](#cli) · [MCP](#mcp) |
| **Extensible sandbox protocol** | Build custom runtime integrations against defined sandbox lifecycle and execution APIs. | [API specs](specs/README.md) |

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

## Examples

Explore examples by what you want your agent to do. Runnable source code lives in [`examples/`](examples/).

| Use case | What you can build | Examples |
|----------|--------------------|----------|
| **Coding agents** | Run coding agents in isolated environments to edit files, execute commands, and complete development tasks. | [Claude Code](docs/examples/claude-code.md) · [Codex CLI](docs/examples/codex-cli.md) · [DeerFlow](docs/examples/deer-flow.md) |
| **Code execution and data analysis** | Execute model-generated code and work with results through the Code Interpreter SDK. | [Code Interpreter](docs/examples/code-interpreter.md) |
| **Browser and desktop automation** | Automate web interactions and testing, or give agents access to a desktop environment. | [Playwright](docs/examples/playwright.md) · [Chrome](docs/examples/chrome.md) · [Desktop](docs/examples/desktop.md) |
| **Agent evaluation** | Run evaluations with a separate sandbox for each trial. | [Harbor Evaluation](docs/examples/harbor-evaluation.md) |

See the [full example catalog](docs/examples/index.md) for more coding agents, framework integrations, remote development environments, Kubernetes deployment, and storage patterns.

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

## Documentation

- [Website](https://open-sandbox.ai) — Project homepage and documentation site
- [Architecture](docs/architecture/index.md) — System design and component responsibilities
- [Deployment Guide](docs/deployment/index.md) — Kubernetes installation and operations
- [Server Configuration](docs/getting-started/configuration.md) — Runtime and server settings
- [SDK Reference](docs/sdks/index.md) — Language guides and capability coverage
- [CLI Guide](cli/README.md) — Installation and command reference
- [MCP Integration](sdks/mcp/sandbox/python/README.md) — Setup for MCP-capable clients
- [API Reference](docs/api/index.md) — Sandbox lifecycle and execution contracts
- [Credential Vault](docs/guides/credential-vault.md) — Outbound credential injection
- [Release Verification](docs/community/release-verification.md) — Image signing and artifact verification
- [Enhancement Proposals](oseps/README.md) — Design proposals and technical direction
- [Roadmap](ROADMAP.md) — Project priorities and planning

## Contact and Discussion

- Issues: Submit bugs, feature requests, or design discussions through GitHub Issues
- Discord: Join the [OpenSandbox Discord community](https://discord.gg/g7FuPs8YeD)
- DingTalk: Join the [OpenSandbox technical discussion group](https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11)

## License

This project is open source under the [Apache 2.0 License](LICENSE).
