# DeerFlow + OpenSandbox Example

Run [DeerFlow](https://github.com/bytedance/deer-flow) agent turns whose file and shell tools execute inside an OpenSandbox sandbox, via the built-in `OpenSandboxProvider` shipped in DeerFlow 2.1.0.

- [main.py](./main.py) — model-free script that drives `OpenSandboxProvider` directly
- [Dockerfile](./Dockerfile) / [entrypoint.sh](./entrypoint.sh) / [enable-opensandbox](./enable-opensandbox) / [opensandbox-server.toml](./opensandbox-server.toml) — all-in-one image bundling the OpenSandbox server (docker runtime), the DeerFlow backend, and the pre-built DeerFlow Web UI (nginx on :2026)

> **Full documentation**: [docs/examples/deer-flow.md](../../docs/examples/deer-flow.md) — see the "All-in-one Docker image" section
