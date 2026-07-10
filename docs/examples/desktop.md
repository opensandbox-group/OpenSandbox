---
title: Desktop (VNC)
description: Launch Xvfb + x11vnc + fluxbox in OpenSandbox to provide a VNC-accessible desktop environment.
---

# Desktop (VNC) Example

Launch Xvfb + x11vnc + fluxbox in OpenSandbox to provide a VNC-accessible desktop environment.

## Build the Desktop Sandbox Image

The Dockerfile in the example directory builds a sandbox image with desktop and VNC components pre-installed:

```shell
cd examples/desktop
docker build -t opensandbox/desktop:latest .
```

This image includes:
- Xvfb (virtual framebuffer X server)
- x11vnc (VNC server)
- XFCE desktop (panel, file manager, terminal)
- Non-root user (desktop) for security

## Start OpenSandbox server [local]

Pre-pull the desktop image:

```shell
docker pull opensandbox/desktop:latest
```

Start the local OpenSandbox server:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the Desktop Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

uv run python examples/desktop/main.py
```

The script starts the desktop stack (Xvfb + XFCE + x11vnc) and also launches noVNC/websockify. It prints:
- VNC endpoint (`endpoint.endpoint`) for native VNC clients, password from `VNC_PASSWORD` (default: `opensandbox`)
- noVNC URL for browsers (`/vnc.html?host=...&port=...&path=...`)

The sandbox stays alive for 5 minutes by default; interrupt sooner with Ctrl+C. Uses the prebuilt desktop image by default.

![Desktop shell](../public/images/desktop-screenshot-shell.jpg)
![noVNC connect](../public/images/desktop-screenshot-connect.jpg)
![noVNC password](../public/images/desktop-screenshot-password.jpg)
![Desktop UI](../public/images/desktop-screenshot-desktop.jpg)

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `opensandbox/desktop:latest` | Sandbox image to use |
| `VNC_PASSWORD` | `opensandbox` | Password for VNC access |

## References

- [noVNC](https://github.com/novnc/noVNC)
- [x11vnc](https://github.com/LibVNC/x11vnc)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/desktop)
