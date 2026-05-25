# 宿主机目录挂载示例

本示例演示如何使用 OpenSandbox Volume API 将宿主机目录挂载到沙箱容器中。宿主机目录挂载支持宿主机与沙箱环境之间的双向文件共享，适用于共享数据集、模型检查点、配置文件或收集沙箱输出等场景。

## 演示场景

| # | 场景 | 说明 |
|---|------|------|
| 1 | **读写挂载** | 挂载宿主机目录，支持双向文件读写 |
| 2 | **只读挂载** | 提供沙箱不可修改的共享数据 |
| 3 | **SubPath 挂载** | 仅挂载宿主机路径下的指定子目录 |

## 前置条件

### 1. 启动 OpenSandbox 服务

```shell
git clone git@github.com:alibaba/OpenSandbox.git
cd OpenSandbox/server
cp opensandbox_server/examples/example.config.zh.toml ~/.sandbox.toml
uv sync && uv run python -m opensandbox_server.main
```

### 2. 配置允许的宿主机路径

出于安全考虑，服务端会限制可挂载的宿主机路径。请在 `~/.sandbox.toml` 中添加 `[storage]` 配置段：

```toml
[storage]
# 允许进行 bind mount 的宿主机路径前缀白名单。
# 仅匹配这些前缀的路径才能被挂载到沙箱中。
# 如果为空，则允许所有路径（不建议在生产环境使用）。
allowed_host_paths = ["/tmp/opensandbox-data", "/data/shared"]
```

> **安全提示**：在生产环境中，请务必设置明确的 `allowed_host_paths`，以防止沙箱访问敏感的宿主机目录。空列表表示允许所有路径，适合本地开发，但不适用于共享环境。

### 3. 创建宿主机目录

```shell
# 创建与沙箱共享的目录
mkdir -p /tmp/opensandbox-data
echo "hello-from-host" > /tmp/opensandbox-data/marker.txt

# 创建用于 subpath 演示的子目录
mkdir -p /tmp/opensandbox-data/datasets/train
echo -e "id,value\n1,100\n2,200\n3,300" > /tmp/opensandbox-data/datasets/train/data.csv
```

### 4. 从源码安装 SDK

Volume 功能需要从源码安装最新版 SDK：

```shell
# 在项目根目录下执行（推荐使用 uv）
uv pip install -e sdks/sandbox/python

# 或者使用 pip（需要在虚拟环境中执行）
# python3 -m venv .venv && source .venv/bin/activate
# pip install -e sdks/sandbox/python
```

### 5. 拉取沙箱镜像

```shell
docker pull registry.cn-hangzhou.aliyuncs.com/acs/ubuntu:latest
```

## 运行

```shell
SANDBOX_IMAGE=registry.cn-hangzhou.aliyuncs.com/acs/ubuntu:latest \
  HOST_VOLUME_PATH=/tmp/opensandbox-data uv run python examples/host-volume-mount/main.py
```

## 预期输出

```text
Using HOST_VOLUME_PATH: /tmp/opensandbox-data

OpenSandbox server : localhost:8080
Sandbox image      : registry.cn-hangzhou.aliyuncs.com/acs/ubuntu:latest
Host volume path   : /tmp/opensandbox-data

============================================================
Scenario 1: Read-Write Host Volume Mount
============================================================
  Host path : /tmp/opensandbox-data
  Mount path: /mnt/shared

  [1] Listing files visible from inside the sandbox:
  total 4
drwxr-xr-x 1 root root 128 Feb  6 09:24 .
drwxr-xr-x 1 root root  12 Feb  6 11:50 ..
drwxr-xr-x 1 root root  96 Feb  6 09:24 datasets
-rw-r--r-- 1 root root  16 Feb  6 09:24 marker.txt

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
  total 8
drwxr-xr-x 1 root root 160 Feb  6 11:50 .
drwxr-xr-x 1 root root  16 Feb  6 11:50 ..
drwxr-xr-x 1 root root  96 Feb  6 09:24 datasets
-rw-r--r-- 1 root root  16 Feb  6 09:24 marker.txt
-rw-r--r-- 1 root root  20 Feb  6 11:50 sandbox-greeting.txt

  [2] Reading marker.txt:
  hello-from-host

  [3] Attempting to write (should fail):
  touch: cannot touch '/mnt/readonly/should-fail.txt': Read-only file system
  Write denied (expected)

  Scenario 2 completed.

============================================================
Scenario 3: SubPath Host Volume Mount
============================================================
  Host path : /tmp/opensandbox-data
  SubPath   : datasets/train
  Mount path: /mnt/training-data

  [1] Listing mounted subpath content:
  total 4
drwxr-xr-x 1 root root 96 Feb  6 09:24 .
drwxr-xr-x 1 root root 26 Feb  6 11:50 ..
-rw-r--r-- 1 root root 27 Feb  6 11:50 data.csv

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

## 各 SDK 用法速览

### Python（异步）

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
            readOnly=False,       # 可选，默认为 False
            subPath="subdir",     # 可选，挂载子目录
        ),
    ],
)
```

### Python（同步）

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

## 参考资料

- [OSEP-0003: Volume 与 VolumeBinding 支持](../../oseps/0003-volume-and-volumebinding-support.md) — 设计提案
- [Sandbox Lifecycle API 规范](../../specs/sandbox-lifecycle.yml) — Volume 定义的 OpenAPI 规范
- [服务端配置示例](../../server/opensandbox_server/examples/example.config.zh.toml) — `[storage]` 段中的 `allowed_host_paths` 配置
