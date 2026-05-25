# Windows Sandbox

A Windows sandbox runs a Windows guest in a Linux container via KVM/QEMU and is managed end-to-end through the OpenSandbox lifecycle API (create, expose endpoints, delete). The current Windows profile is based on [`dockur/windows`](https://github.com/dockur/windows), with service integration and capabilities provided by OpenSandbox.

## Scope

This guide covers the OpenSandbox Server Windows profile (`platform.os=windows`):

- Create a Windows sandbox
- Check status and endpoints
- Common resources and volume settings
- Delete and cleanup

## How it works

- **Runtime**: [`dockur/windows`](https://github.com/dockur/windows) runs the Windows guest in a Linux container (requires KVM/QEMU), using mount points such as `/storage` and `/boot.iso` for the system disk and install media.  
- **Control plane**: On create, OpenSandbox Server injects the Windows profile configuration (devices, capabilities, `execd`, port mappings) and enforces resource checks, state, and endpoint publication.  
- **Client**: You call the Lifecycle API or the Python SDK with `platform.os=windows`, then follow create → poll `Running` → get endpoints → use → delete.

## Prerequisites

- Reachable OpenSandbox Server (e.g. `http://localhost:8080`)
- Host meets the Windows profile requirements:
  - `/dev/kvm` present
  - `/dev/net/tun` present
- Server `storage.allowed_host_paths` configured for allowed host paths
- A host directory to bind to `/storage` (recommended)
- Optional: a local Windows ISO for `/boot.iso` (cuts repeated download time)

## Key settings and constraints

### Platform and image

- Set `platform: {"os":"windows","arch":"amd64"}` (`arm64` depends on the host)
- Suggested image: `dockurr/windows:latest`; see [`dockur/windows` releases](https://github.com/dockur/windows/releases) and [Docker Hub tags](https://hub.docker.com/r/dockurr/windows/tags)

### Minimum resources

The Windows profile enforces:

- `resourceLimits.cpu >= 2`
- `resourceLimits.memory >= 4G`
- `resourceLimits.disk >= 64G`

### `resourceLimits` vs. environment variables

If `resourceLimits` is set, the server maps it to and overwrites these env vars:

- `CPU_CORES`
- `RAM_SIZE`
- `DISK_SIZE`

Prefer `resourceLimits` only; do not set those three env vars manually.

### Default ports and `USER_PORTS`

The Windows profile wires these ports by default:

- `44772` (execd)
- `8080`
- `3389/tcp`, `3389/udp` (RDP)
- `8006/tcp` (typical web console)

It also builds or merges `USER_PORTS` (often `44772,8080,3389,8006`).

### Storage mounts

- Bind a writable host directory to `/storage`
- Optional: bind an ISO file to `/boot.iso` (`readOnly=true`)
- Host paths must fall under `storage.allowed_host_paths` or the request is rejected

## End-to-end example (Python)

This flow:

1. Create a Windows sandbox  
2. Wait until `Running`  
3. Get the `44772` endpoint  
4. Use the Python SDK against the sandbox  
5. Delete the sandbox  

### Minimal example

```python
import os
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import PlatformSpec

BASE_URL = os.getenv("OPEN_SANDBOX_BASE_URL", "http://localhost:8080")
API_KEY = os.getenv("OPEN_SANDBOX_API_KEY", "")


def main() -> None:
    cfg = ConnectionConfigSync(
        domain=BASE_URL,
        api_key=API_KEY or None,
        use_server_proxy=True,
    )

    sbx = None
    try:
        sbx = SandboxSync.create(
            image="dockurr/windows:latest",
            timeout=timedelta(hours=12),
            ready_timeout=timedelta(minutes=30),
            resource={
                "cpu": "4",
                "memory": "8G",
                "disk": "64G",
            },
            env={"VERSION": "11"},
            entrypoint=["cmd", "/c", "echo OpenSandbox Windows profile"],
            platform=PlatformSpec(os="windows", arch="amd64"),
            connection_config=cfg,
        )
        print("created:", sbx.id)

        endpoint = sbx.get_endpoint(44772)
        print("execd endpoint:", endpoint.endpoint)
        print("sdk health:", sbx.is_healthy())
    finally:
        if sbx is not None:
            try:
                sbx.kill()
                print("deleted:", sbx.id)
            finally:
                sbx.close()


if __name__ == "__main__":
    main()
```

> The sample keeps SDK health waiting (`ready_timeout=30m`). Windows cold start can be slow; increase `ready_timeout` as needed. For custom readiness logic, use `skip_health_check=True` and poll for `Running` in your app.

### Advanced options

#### Persistent `/storage`

Bind a host path to `/storage` for a persistent system/user data directory. The directory **need not exist** ahead of time: it may be created on bind mount (fresh), or reused if it already exists. `host.path` must still be under an allowed `storage.allowed_host_paths` prefix, regardless of whether the directory exists.

```python
from opensandbox.models.sandboxes import Host, Volume

volumes = [
    Volume(
        name="win-storage",
        host=Host(path="/data/opensandbox/windows-storage"),
        mount_path="/storage",
        read_only=False,
    ),
]
# Pass as SandboxSync.create(..., volumes=volumes, ...)
```

#### Existing ISO on the host

Bind a local Windows install ISO read-only to `/boot.iso` to avoid downloading media each time (also requires `allowed_host_paths`; the path must be a **file**):

```python
from opensandbox.models.sandboxes import Host, Volume

volumes = [
    Volume(
        name="win-iso",
        host=Host(path="/data/iso/Win11_23H2.iso"),
        mount_path="/boot.iso",
        read_only=True,
    ),
]
```

#### Customizing `dockur/windows`

[`dockur/windows`](https://github.com/dockur/windows) is configured with **environment variables**. OpenSandbox passes your `env` into the container as documented upstream. **Install / UX** settings often include:

- `VERSION`: Windows variant code (e.g. `11`, `11l`, `10` — full table in the project README) or, in some cases, a custom ISO `https://` URL  
- `USERNAME` / `PASSWORD`: first user and password (defaults in the image if omitted — see upstream)  
- `LANGUAGE`: install language (e.g. `English`, `Chinese`)  
- `REGION` / `KEYBOARD`: locale and layout (e.g. `en-US`, `zh-CN`)  
- Advanced cases may use `DHCP`, `ARGUMENTS` (extra QEMU args), etc. — see the [`dockur/windows` FAQ](https://github.com/dockur/windows)

If you already set CPU, memory, and disk via `resourceLimits`, do **not** repeat the same **resource** keys in `env` as in the dockur docs; see [`resourceLimits` vs. environment variables](#resourcelimits-vs-environment-variables) above. Other non-resource keys can be combined freely.

```python
env = {
    "VERSION": "11l",
    "USERNAME": "Docker",
    "PASSWORD": "your-secure-password",
    "LANGUAGE": "Chinese",
    "REGION": "zh-CN",
    "KEYBOARD": "zh-CN",
}
```

For the full list, defaults, and interactions, use the [`dockur/windows` repository](https://github.com/dockur/windows) and image tags.

#### Using your own Windows image

In [`dockur/windows` documentation](https://github.com/dockur/windows), **“your own”** usually means **which Windows install ISO to use** — not a custom recipe unrelated to upstream. The [custom install FAQ](https://github.com/dockur/windows#how-do-i-install-a-custom-image) states:

- For an ISO **not** in the project’s version table, set `VERSION` to the ISO’s **download URL** (`https://...`); the container fetches and installs it.  
- If you **already have a local ISO file**, bind it to **`/boot.iso`** as in [Existing ISO on the host](#existing-iso-on-the-host) above; upstream documents that `VERSION` is then ignored.

In OpenSandbox, set `VERSION` in `env` and, when needed, add a volume for the local ISO at `/boot.iso`, matching the upstream flow; keep `image` as `dockurr/windows` or another compatible upstream image, as in the other examples.

Snippet for the **`VERSION` = ISO URL** case (same as passing `env` in the minimal example):

```python
env = {
    "VERSION": "https://example.com/path/to/your.iso",
    # same pattern as the minimal example for resource / platform / etc.
}
```

## FAQ

- `Unsupported platform.os 'windows'` on create — server build has no Windows profile; upgrade to a version that includes it.

- `INVALID_PARAMETER` for `resourceLimits.*` — ensure `cpu >= 2`, `memory >= 4G`, `disk >= 64G`.

- Stays `Pending` a long time — first Windows install is slow; check host resources and `/storage` space, and increase wait timeouts.

- Status `Running` but endpoint unreachable — verify `GET /v1/sandboxes/{id}/endpoints/44772` returns a valid address; set `env.USER_PORTS` if you need more ports forwarded.
