---
layout: home

hero:
  name: OpenSandbox
  text: Universal Sandbox Infrastructure for AI Applications
  tagline: Securely run commands, filesystems, code interpreters, browsers, and developer tools in isolated runtime environments.
  actions:
    - theme: brand
      text: Quick Start
      link: /overview/home
    - theme: alt
      text: Explore Architecture
      link: /overview/architecture

features:
  - title: Sandbox Lifecycle and Runtime Management
    details: Provision, monitor, renew, and terminate sandbox instances with Docker and Kubernetes-oriented runtime capabilities.
  - title: Multi-Language SDKs and Unified APIs
    details: Build with Python, Java/Kotlin, and JavaScript SDKs on top of standardized lifecycle and execution protocols.
  - title: Powerful In-Sandbox Execution
    details: Execute shell commands, manage files, run multi-language code interpreters, expose ports, and stream logs/metrics.
  - title: Built for Real AI Workloads
    details: Supports coding agents, browser automation, remote development, AI code execution, and RL training scenarios.
---

## Typical Scenarios

OpenSandbox is now listed in the [CNCF Landscape](https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox).

<div class="scenario-grid">
  <a class="scenario-card" href="./examples/claude-code/readme">
    <h3>Coding Agents</h3>
    <p>Run Claude Code, Gemini CLI, Codex, and other agent tools in isolated sandboxes.</p>
  </a>
  <a class="scenario-card" href="./examples/playwright/readme">
    <h3>Browser Automation</h3>
    <p>Execute Chrome and Playwright workloads with controlled runtime, filesystem, and networking.</p>
  </a>
  <a class="scenario-card" href="./examples/vscode/readme">
    <h3>Remote Development</h3>
    <p>Host VS Code Web and desktop-like environments for secure cloud development workflows.</p>
  </a>
  <a class="scenario-card" href="./examples/code-interpreter/readme">
    <h3>AI Code Execution</h3>
    <p>Run model-generated code safely, stream outputs, and iterate quickly with reproducible environments.</p>
  </a>
  <a class="scenario-card" href="./examples/rl-training/readme">
    <h3>RL Training</h3>
    <p>Launch reinforcement learning tasks with managed sandbox lifecycle and resource controls.</p>
  </a>
</div>

Explore all scenario references in [Examples](./examples/readme).
