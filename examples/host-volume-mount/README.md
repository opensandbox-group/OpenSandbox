# Host Volume Mount Example

This example demonstrates how to mount host directories into sandbox containers using the OpenSandbox Volume API. Host volume mounts enable bidirectional file sharing between the host machine and sandbox environments — ideal for sharing datasets, model checkpoints, configuration files, or collecting sandbox outputs.

## Scenarios

| # | Scenario | Description |
|---|----------|-------------|
| 1 | **Read-write mount** | Mount a host directory for bidirectional file exchange |
| 2 | **Read-only mount** | Provide shared data that sandboxes cannot modify |
| 3 | **SubPath mount** | Mount a specific subdirectory from the host path |

## Prerequisites

### 1. Start OpenSandbox Server

```shell
git clone git@github.com:alibaba/OpenSandbox.git
cd OpenSandbox/server
cp opensandbox_server/examples/example.config.toml ~/.sandbox.toml
uv sync && uv run python -m opensandbox_server.main
```

### 2. Configure Allowed Host Paths

For security, the server restricts which host paths can be mounted. Add a `[storage]` section to `~/.sandbox.toml`:

```toml
[storage]
# Allowlist of host path prefixes permitted for bind mounts.
# Only paths under these prefixes can be mounted into sandboxes.
# If empty, all host paths are allowed (not recommended for production).
allowed_host_paths = ["/tmp/opensandbox-data", "/data/shared"]
```

> **Security note**: In production, always set explicit `allowed_host_paths` to prevent sandboxes from accessing sensitive host directories. An empty list allows all paths, which is convenient for local development but not safe for shared environments.

### 3. Create Host Directories

```shell
# Create a directory to share with sandboxes
mkdir -p /tmp/opensandbox-data
echo "hello-from-host" > /tmp/opensandbox-data/marker.txt

# Create a subdirectory for the subpath demo
mkdir -p /tmp/opensandbox-data/datasets/train
echo -e "id,value\n1,100\n2,200\n3,300" > /tmp/opensandbox-data/datasets/train/data.csv
```

### 4. Install SDK from Source

Volume support requires the latest SDK built from source (not yet available in the released package):

```shell
# From the project root (recommended: use uv)
uv pip install -e sdks/sandbox/python

# Or use pip inside a virtual environment
# python3 -m venv .venv && source .venv/bin/activate
# pip install -e sdks/sandbox/python
```

### 5. Pull the Sandbox Image

```shell
docker pull ubuntu:latest
```

## Run

```shell
HOST_VOLUME_PATH=/tmp/opensandbox-data uv run python examples/host-volume-mount/main.py
```

## Expected Output

```text
Using HOST_VOLUME_PATH: /tmp/opensandbox-data

OpenSandbox server : localhost:8080
Sandbox image      : ubuntu
Host volume path   : /tmp/opensandbox-data

============================================================
Scenario 1: Read-Write Host Volume Mount
============================================================
  Host path : /tmp/opensandbox-data
  Mount path: /mnt/shared

  [1] Listing files visible from inside the sandbox:
  total 12
  drwxrwxrwx 3 root root 4096 ... .
  drwxr-xr-x 1 root root 4096 ... ..
  -rw-r--r-- 1 root root   16 ... marker.txt
  drwxr-xr-x 3 root root 4096 ... datasets

  [2] Writing a file from inside the sandbox:
  -> Written: /mnt/shared/sandbox-greeting.txt

  [3] Reading back the file:
  Hello from sandbox!

  [4] Verified on host: /tmp/opensandbox-data/sandbox-greeting.txt
      Content: Hello from sandbox!

  Scenario 1 completed.

============================================================
Scenario 2: Read-Only Host Volume Mount
============================================================
  Host path : /tmp/opensandbox-data
  Mount path: /mnt/readonly

  [1] Reading files from read-only mount:
  ...

  [2] Reading marker.txt:
  hello-from-host

  [3] Attempting to write (should fail):
  Write denied (expected)

  Scenario 2 completed.

============================================================
Scenario 3: SubPath Host Volume Mount
============================================================
  Host path : /tmp/opensandbox-data
  SubPath   : datasets/train
  Mount path: /mnt/training-data

  [1] Listing mounted subpath content:
  ...
  -rw-r--r-- 1 root root   28 ... data.csv

  [2] Reading data.csv:
  id,value
  1,100
  2,200
  3,300

  Scenario 3 completed.

============================================================
All scenarios completed successfully!
============================================================
```

## SDK Usage Quick Reference

### Python (async)

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import Host, Volume

sandbox = await Sandbox.create(
    image="ubuntu",
    volumes=[
        Volume(
            name="my-data",
            host=Host(path="/data/shared"),
            mountPath="/mnt/data",
            readOnly=False,       # optional, default is False
            subPath="subdir",     # optional, mount a subdirectory
        ),
    ],
)
```

### Python (sync)

```python
from opensandbox import SandboxSync
from opensandbox.models.sandboxes import Host, Volume

sandbox = SandboxSync.create(
    image="ubuntu",
    volumes=[
        Volume(
            name="my-data",
            host=Host(path="/data/shared"),
            mountPath="/mnt/data",
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
      host: { path: "/data/shared" },
      mountPath: "/mnt/data",
      readOnly: false,
    },
  ],
});
```

### Java / Kotlin

```java
Volume volume = Volume.builder()
    .name("my-data")
    .host(Host.of("/data/shared"))
    .mountPath("/mnt/data")
    .readOnly(false)
    .build();

Sandbox sandbox = Sandbox.builder()
    .image("ubuntu")
    .volume(volume)
    .build();
```

## References

- [OSEP-0003: Volume and VolumeBinding Support](../../oseps/0003-volume-and-volumebinding-support.md) — Design proposal
- [Sandbox Lifecycle API Spec](../../specs/sandbox-lifecycle.yml) — OpenAPI schema for volume definitions
- [Server Configuration](../../server/opensandbox_server/examples/example.config.toml) — `[storage]` section for `allowed_host_paths`
