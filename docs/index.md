---
layout: home

hero:
  name: OpenSandbox
  text: Universal Sandbox Infrastructure for AI Applications
  tagline: Securely run commands, code interpreters, browsers, and developer tools in isolated environments with multi-language SDKs.
  actions:
    - theme: brand
      text: Quick Start
      link: /getting-started/
    - theme: alt
      text: Architecture
      link: /architecture/
    - theme: alt
      text: SDKs
      link: /sdks/

features:
  - title: Sandbox Lifecycle Management
    details: Provision, monitor, renew, pause/resume, and terminate sandbox instances with Docker and Kubernetes runtimes.
  - title: Multi-Language SDKs
    details: Build with Python, Java/Kotlin, JavaScript/TypeScript, C#/.NET, and Go SDKs on top of standardized lifecycle and execution protocols.
  - title: In-Sandbox Execution
    details: Execute shell commands, manage files, run multi-language code interpreters, expose ports, and stream logs and metrics.
  - title: Built for AI Workloads
    details: Supports coding agents, browser automation, remote development, code execution, and reinforcement learning scenarios.
---

## Typical Scenarios

OpenSandbox is listed in the [CNCF Landscape](https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox).

<div class="scenario-grid">
  <a class="scenario-card" href="/examples/claude-code">
    <h3>Coding Agents</h3>
    <p>Run Claude Code, Gemini CLI, Codex, and other coding agents in isolated sandboxes.</p>
  </a>
  <a class="scenario-card" href="/examples/playwright">
    <h3>Browser Automation</h3>
    <p>Execute Chrome and Playwright workloads with controlled runtime, filesystem, and networking.</p>
  </a>
  <a class="scenario-card" href="/examples/vscode">
    <h3>Remote Development</h3>
    <p>Host VS Code Web and desktop-like environments for secure cloud development workflows.</p>
  </a>
  <a class="scenario-card" href="/examples/code-interpreter">
    <h3>AI Code Execution</h3>
    <p>Run model-generated code safely, stream outputs, and iterate quickly with reproducible environments.</p>
  </a>
  <a class="scenario-card" href="/examples/rl-training">
    <h3>RL Training</h3>
    <p>Launch reinforcement learning tasks with managed sandbox lifecycle and resource controls.</p>
  </a>
</div>

Explore all examples in [Examples](/examples/).

## Quick Install

::: code-group

```bash [Python SDK]
pip install opensandbox
```

```bash [JavaScript SDK]
npm install @alibaba-group/opensandbox
```

```kotlin [Kotlin (Gradle)]
dependencies {
    implementation("com.alibaba.opensandbox:sandbox:{latest_version}")
}
```

```bash [Go SDK]
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

```bash [C# SDK]
dotnet add package Alibaba.OpenSandbox
```

:::

See [Getting Started](/getting-started/) for the full setup guide.
