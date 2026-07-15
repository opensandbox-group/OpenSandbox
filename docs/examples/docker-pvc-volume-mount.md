---
title: Docker PVC Volume
description: Mount Docker named volumes into sandbox containers using the OpenSandbox pvc backend.
---

# Docker PVC (Named Volume) Mount Example

This example demonstrates how to mount Docker named volumes into sandbox containers using the OpenSandbox `pvc` backend. In Docker runtime, `pvc.claimName` maps to a Docker named volume -- providing a more convenient and secure alternative to host-path bind mounts for sharing data across sandboxes.

::: info What is `pvc`?
The `pvc` backend is a runtime-neutral abstraction. In Kubernetes it maps to a PersistentVolumeClaim; in Docker it maps to a named volume. The same API request works on both runtimes. See [OSEP-0003](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0003-volume-and-volumebinding-support.md) for the design.
:::

## Why Named Volumes over Host Paths?

| | Host path (`host` backend) | Named volume (`pvc` backend) |
|---|---|---|
| **Security** | Exposes host filesystem paths | Docker manages storage location; no host path exposed |
| **Setup** | Requires `allowed_host_paths` allowlist | No allowlist needed |
| **Cross-sandbox sharing** | All containers must agree on a host path | Reference the same volume name |
| **Portability** | Tied to host directory structure | Works on any Docker host |
| **Lifecycle** | User manages host directories | `docker volume create/rm` |

## Scenarios

| # | Scenario | Description |
|---|----------|-------------|
| 1 | **Read-write mount** | Mount a named volume for bidirectional file I/O |
| 2 | **Read-only mount** | Mount a named volume that sandboxes cannot modify |
| 3 | **Cross-sandbox sharing** | Two sandboxes share data through the same named volume |
| 4 | **SubPath mount** | Mount only a subdirectory of a named volume (consistent with K8s PVC subPath) |

## Prerequisites

### 1. Start OpenSandbox Server

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

### 2. Create a Docker Named Volume

```shell
# Create the named volume
docker volume create opensandbox-pvc-demo

# Seed it with a marker file via a temporary container
docker run --rm -v opensandbox-pvc-demo:/data alpine \
  sh -c "echo 'hello-from-named-volume' > /data/marker.txt"
```

### 3. Install Python SDK

```shell
uv pip install opensandbox
```

### 4. Pull the Sandbox Image

```shell
docker pull ubuntu:latest
```

## Run

```shell
uv run python examples/docker-pvc-volume-mount/main.py
```

The script automatically creates the named volume and seeds it with test data. You can also specify a custom volume name or image:

```shell
SANDBOX_IMAGE=ubuntu SANDBOX_DOMAIN=localhost:8080 uv run python examples/docker-pvc-volume-mount/main.py
```

## Expected Output

```text
OpenSandbox server : localhost:8080
Sandbox image      : ubuntu
Docker volume      : opensandbox-pvc-demo
  Ensuring Docker named volume 'opensandbox-pvc-demo' exists...
  Created volume 'opensandbox-pvc-demo' with marker.txt

============================================================
Scenario 1: Read-Write PVC (Named Volume) Mount
============================================================
  ...

============================================================
Scenario 2: Read-Only PVC (Named Volume) Mount
============================================================
  ...

============================================================
Scenario 3: Cross-Sandbox Sharing via PVC (Named Volume)
============================================================
  ...

============================================================
Scenario 4: SubPath PVC (Named Volume) Mount
============================================================
  ...

============================================================
All scenarios completed successfully!
============================================================
```

## SDK Usage Quick Reference

### Python (async)

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import PVC, Volume

sandbox = await Sandbox.create(
    image="ubuntu",
    volumes=[
        Volume(
            name="my-data",
            pvc=PVC(claimName="my-named-volume"),
            mountPath="/mnt/data",
            readOnly=False,       # optional, default is False
            subPath="datasets/train",  # optional, mount a subdirectory
        ),
    ],
)
```

### Python (sync)

```python
from opensandbox import SandboxSync
from opensandbox.models.sandboxes import PVC, Volume

sandbox = SandboxSync.create(
    image="ubuntu",
    volumes=[
        Volume(
            name="my-data",
            pvc=PVC(claimName="my-named-volume"),
            mountPath="/mnt/data",
            subPath="datasets/train",  # optional
        ),
    ],
)
```

### JavaScript / TypeScript

```typescript
import { Sandbox } from "@alibaba-group/opensandbox";

const sandbox = await Sandbox.create({
  image: "ubuntu",
  volumes: [
    {
      name: "my-data",
      pvc: { claimName: "my-named-volume" },
      mountPath: "/mnt/data",
      readOnly: false,
      subPath: "datasets/train",  // optional
    },
  ],
});
```

### Java / Kotlin

```java
Volume volume = Volume.builder()
    .name("my-data")
    .pvc(PVC.of("my-named-volume"))
    .mountPath("/mnt/data")
    .readOnly(false)
    .subPath("datasets/train")  // optional
    .build();

Sandbox sandbox = Sandbox.builder()
    .image("ubuntu")
    .volume(volume)
    .build();
```

## Cleanup

```shell
docker volume rm opensandbox-pvc-demo
```

## References

- [OSEP-0003: Volume and VolumeBinding Support](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0003-volume-and-volumebinding-support.md) -- Design proposal
- [Sandbox Lifecycle API Spec](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml) -- OpenAPI schema for volume definitions
- [Host Volume Mount Example](/examples/host-volume-mount) -- Host path bind mount example (alternative approach)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/docker-pvc-volume-mount)
