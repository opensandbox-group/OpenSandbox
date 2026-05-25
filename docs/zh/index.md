---
layout: home

hero:
  name: OpenSandbox
  text: 面向 AI 应用的通用沙箱基础设施
  tagline: 在隔离运行时中安全执行命令、文件操作、代码解释器、浏览器与开发工具。
  actions:
    - theme: brand
      text: 快速开始
      link: /zh/overview/home
    - theme: alt
      text: 查看架构
      link: /zh/overview/architecture

features:
  - title: 沙箱全生命周期与运行时管理
    details: 支持沙箱实例创建、监控、续期与销毁，覆盖 Docker 与 Kubernetes 场景。
  - title: 多语言 SDK 与统一协议
    details: 提供 Python、Java/Kotlin、JavaScript SDK，并基于统一的生命周期与执行协议进行开发。
  - title: 强大的沙箱内执行能力
    details: 支持命令执行、文件系统操作、多语言代码解释、端口暴露以及日志/指标流式获取。
  - title: 面向真实 AI 工作负载
    details: 适配 Coding Agent、浏览器自动化、远程开发、AI 代码执行与强化学习训练等场景。
---

## 典型落地场景

OpenSandbox 已进入 [CNCF Landscape](https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox)。

<div class="scenario-grid">
  <a class="scenario-card" href="./examples/claude-code/readme">
    <h3>Coding Agent</h3>
    <p>在隔离沙箱中运行 Claude Code、Gemini CLI、Codex 等工具链。</p>
  </a>
  <a class="scenario-card" href="./examples/playwright/readme">
    <h3>浏览器自动化</h3>
    <p>运行 Chrome、Playwright 等工作负载，结合可控运行时、文件系统与网络策略。</p>
  </a>
  <a class="scenario-card" href="./examples/vscode/readme">
    <h3>远程开发环境</h3>
    <p>提供 VS Code Web 与桌面化开发环境，提升云端开发的安全性与一致性。</p>
  </a>
  <a class="scenario-card" href="./examples/code-interpreter/readme">
    <h3>AI 代码执行</h3>
    <p>安全执行模型生成代码，流式采集输出并在可复现环境中快速迭代。</p>
  </a>
  <a class="scenario-card" href="./examples/rl-training/readme">
    <h3>强化学习训练</h3>
    <p>在可控资源下运行 RL 训练任务，并利用沙箱生命周期能力管理训练过程。</p>
  </a>
</div>

更多场景请查看 [示例](./examples/readme)。
