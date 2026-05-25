# OpenSandbox Examples

Examples for common OpenSandbox use cases. Each subdirectory contains runnable code and documentation.

## Integrations / Sandboxes
- 🧰 [**aio-sandbox**](aio-sandbox): All-in-one sandbox setup using OpenSandbox SDK and agent-sandbox
- <img src="https://kubernetes.io/icons/favicon-32.png" alt="Kubernetes" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**agent-sandbox**](agent-sandbox): Create a kubernetes-sigs/agent-sandbox instance and run a command
- 🧪 [**code-interpreter**](code-interpreter): Code Interpreter SDK singleton example
- 💾 [**host-volume-mount**](host-volume-mount): Mount host directories into sandboxes (read-write, read-only, subpath)
- 📦 [**docker-pvc-volume-mount**](docker-pvc-volume-mount): Mount Docker named volumes via the `pvc` backend (parity with Kubernetes PVC API)
- ☁️ [**docker-ossfs-volume-mount**](docker-ossfs-volume-mount): Mount OSSFS volumes in Docker runtime (inline credentials, subpath, sharing)
- <img src="https://kubernetes.io/icons/favicon-32.png" alt="Kubernetes" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**kubernetes-pvc-volume-mount**](kubernetes-pvc-volume-mount): Mount Kubernetes PersistentVolumeClaims into sandboxes for persistent storage
- 🎯 [**rl-training**](rl-training): Reinforcement learning training loop inside a sandbox
- <img src="https://img.shields.io/badge/-%20-D97757?logo=claude&logoColor=white&style=flat-square" alt="Claude" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**claude-code**](claude-code): Call Claude (Anthropic) API/CLI within the sandbox
- <img src="https://geminicli.com/favicon.ico" alt="Google Gemini" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**gemini-cli**](gemini-cli): Call Google Gemini within the sandbox
- <img src="https://developers.openai.com/favicon.png" alt="OpenAI" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**codex-cli**](codex-cli): Call OpenAI/Codex-like models within the sandbox
- <img src="https://avatars.githubusercontent.com/u/159934110?s=32&v=4" alt="Qwen" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**qwen-code**](qwen-code): Run Qwen Code inside the sandbox
- <img src="https://www.kimi.com/favicon.ico" alt="Kimi" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**kimi-cli**](kimi-cli): Call Kimi Code CLI (Moonshot AI) within the sandbox
- <img src="https://img.shields.io/badge/-%20-1C3C3C?logo=langgraph&logoColor=white&style=flat-square" alt="LangGraph" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**langgraph**](langgraph): LangGraph agent orchestrating sandbox lifecycle + tools
- <img src="https://google.github.io/adk-docs/assets/agent-development-kit.png" alt="Google ADK" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**google-adk**](google-adk): Google ADK agent calling OpenSandbox tools
- 🦞 [**nullclaw**](nullclaw): Launch a Nullclaw Gateway inside a sandbox
- 🦞 [**openclaw**](openclaw): Run an OpenClaw Gateway inside a sandbox
- 🖥️ [**desktop**](desktop): Launch VNC desktop (Xvfb + x11vnc) for VNC client connections
- 🪟 [**windows**](windows): Run a Windows guest VM via KVM/QEMU with RDP and web console access
- <img src="https://playwright.dev/img/playwright-logo.svg" alt="Playwright" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**playwright**](playwright): Launch headless browser (Playwright + Chromium) to scrape web content
- <img src="https://code.visualstudio.com/assets/favicon.ico" alt="VS Code" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**vscode**](vscode): Launch code-server (VS Code Web) to provide browser access
- <img src="https://www.google.com/chrome/static/images/chrome-logo.svg" alt="Google Chrome" width="16" height="16" style="display:inline-block;width:16px;height:16px;vertical-align:middle;margin-right:4px;" /> [**chrome**](chrome): Launch headless Chromium with DevTools port exposed for remote debugging

## How to Run
- Set basic environment variables (e.g., `export SANDBOX_DOMAIN=...`, `export SANDBOX_API_KEY=...`)
- Add provider-specific variables as needed (e.g., `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `API_KEY` for Qwen Code, `KIMI_API_KEY`, etc.; model selection is optional)
- Navigate to the example directory and install dependencies: `pip install -r requirements.txt` (or refer to the Dockerfile in the directory)
- Then execute: `python main.py`
- To run in a container, build and run using the `Dockerfile` in the directory
- Summary: First set required environment variables via `export`, then run `python main.py` in the corresponding directory, or build/run the Docker image for that directory.
