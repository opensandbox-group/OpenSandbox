# Docker OSSFS Volume Mount Example

This example demonstrates how to use the new SDK `ossfs` volume model to mount Alibaba Cloud OSS into sandboxes on Docker runtime.

## What this example covers

1. **Basic read-write mount** on an OSSFS backend.
2. **Cross-sandbox sharing** on the same OSSFS backend path.
3. **Two mounts, different OSS prefixes via `subPath`**.

## Prerequisites

### 1) Start OpenSandbox server (Docker runtime)

Make sure your server host has:

- Linux host OS (OSSFS backend is not supported when OpenSandbox Server runs on Windows)
- `ossfs` installed
- FUSE support enabled
- writable local mount root for OSSFS (default `storage.ossfs_mount_root=/mnt/ossfs`)

`storage.ossfs_mount_root` is **optional** if you use the default `/mnt/ossfs`.
Even with on-demand mounting, the runtime still needs a deterministic host-side
base directory to place dynamic mounts (`<mount_root>/<bucket>/<subPath?>`).

Optional config example:

```toml
[runtime]
type = "docker"

[storage]
ossfs_mount_root = "/mnt/ossfs"
```

Then start the server:

```bash
opensandbox-server
```

### 2) Install Python SDK

```bash
uv pip install opensandbox
```

If your PyPI version does not include OSSFS volume models yet, install from source:

```bash
pip install -e sdks/sandbox/python
```

### 3) Prepare OSS credentials and target path

```bash
export SANDBOX_DOMAIN=localhost:8080
export SANDBOX_API_KEY=your-api-key
export SANDBOX_IMAGE=ubuntu

export OSS_BUCKET=your-bucket
export OSS_ENDPOINT=oss-cn-hangzhou.aliyuncs.com
export OSS_ACCESS_KEY_ID=your-ak
export OSS_ACCESS_KEY_SECRET=your-sk
```

## Run

```bash
uv run python examples/docker-ossfs-volume-mount/main.py
```

## Minimal SDK usage snippet

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import OSSFS, Volume

sandbox = await Sandbox.create(
    image="ubuntu",
    volumes=[
        Volume(
            name="oss-data",
            ossfs=OSSFS(
                bucket="your-bucket",
                endpoint="oss-cn-hangzhou.aliyuncs.com",
                # version="2.0",   # optional, default is "2.0"
                accessKeyId="your-ak",
                accessKeySecret="your-sk",
            ),
            mountPath="/mnt/data",
            subPath="train",      # optional
            readOnly=False,       # optional
        )
    ],
)
```

## Notes

- Current implementation supports **inline credentials only** (`accessKeyId`/`accessKeySecret`).
- Mounting is **on-demand** in Docker runtime (mount-or-reuse), not pre-mounted for all buckets.
- `ossfs.version` exists in API/SDK with enum `"1.0" | "2.0"`, and defaults to `"2.0"` when omitted.
- Docker runtime now applies **version-specific mount argument encoding**:
  - `1.0`: mounts via `ossfs ... -o <option>`.
  - `2.0`: mounts via `ossfs2 mount ... -c <config-file>` where options are written as `--<option>` config lines.
- `options` values must be **raw payloads** without leading `-` (for example: `allow_other`, `umask=0022`).

## References

- [OSEP-0003: Volume and VolumeBinding Support](../../oseps/0003-volume-and-volumebinding-support.md)
- [Sandbox Lifecycle API Spec](../../specs/sandbox-lifecycle.yml)
