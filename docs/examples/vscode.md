---
title: VS Code
description: Launch code-server (VS Code Web) in OpenSandbox to provide browser-based IDE access.
---

# VS Code Example

Launch code-server (VS Code Web) in OpenSandbox to provide browser access.

## Build the VS Code Sandbox Image

The Dockerfile in the example directory builds a sandbox image with code-server pre-installed:

```shell
cd examples/vscode
docker build -t opensandbox/vscode:latest .
```

This image includes:
- code-server (VS Code Web) pre-installed
- Non-root user (vscode) for security
- Workspace directory at `/workspace`

## Start OpenSandbox server [local]

Pre-pull the VS Code image:

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/vscode:latest
```

Start the local OpenSandbox server:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the VS Code Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

uv run python examples/vscode/main.py
```

The script starts code-server (with authentication disabled), binds it to the specified port and outputs the accessible address. Uses the prebuilt VS Code image by default.

![VS Code screenshot shell](../public/images/vscode-screenshot-shell.jpg)
![VS Code screenshot vscode](../public/images/vscode-screenshot-vscode.jpg)

## References

- [code-server (VS Code Web)](https://github.com/coder/code-server)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/vscode)
