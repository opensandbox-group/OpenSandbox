# Docker OSSFS 挂载示例

本示例演示如何使用新版 SDK 的 `ossfs` volume 模型，在 Docker 运行时将阿里云 OSS 挂载到沙箱容器。

## 覆盖场景

1. **基础读写挂载**（OSSFS backend）。
2. **跨沙箱共享数据**（同一 OSSFS backend path）。
3. **通过 `subPath` 挂载不同 OSS prefix**。

## 前置条件

### 1) 启动 OpenSandbox 服务（Docker runtime）

请确保服务端主机满足：

- Linux 主机系统（OpenSandbox Server 运行在 Windows 时不支持 OSSFS backend）
- 已安装 `ossfs`
- 已启用 FUSE
- 已有可写的 OSSFS 本地挂载根目录（默认 `storage.ossfs_mount_root=/mnt/ossfs`）

`storage.ossfs_mount_root` 是**可选配置**（使用默认值时可不写）。
即使是按需动态挂载，运行时仍需要一个确定的宿主机根目录来放置挂载点：
`<mount_root>/<bucket>/<subPath?>`。

可选配置示例：

```toml
[runtime]
type = "docker"

[storage]
ossfs_mount_root = "/mnt/ossfs"
```

启动服务：

```bash
opensandbox-server
```

### 2) 安装 Python SDK

```bash
uv pip install opensandbox
```

如果当前 PyPI 版本还不包含 OSSFS 相关模型，可从源码安装：

```bash
pip install -e sdks/sandbox/python
```

### 3) 配置 OSS 参数

```bash
export SANDBOX_DOMAIN=localhost:8080
export SANDBOX_API_KEY=your-api-key
export SANDBOX_IMAGE=ubuntu

export OSS_BUCKET=your-bucket
export OSS_ENDPOINT=oss-cn-hangzhou.aliyuncs.com
export OSS_ACCESS_KEY_ID=your-ak
export OSS_ACCESS_KEY_SECRET=your-sk
```

## 运行

```bash
uv run python examples/docker-ossfs-volume-mount/main.py
```

## SDK 最小示例

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
                # version="2.0",   # 可选，默认 "2.0"
                accessKeyId="your-ak",
                accessKeySecret="your-sk",
            ),
            mountPath="/mnt/data",
            subPath="train",      # 可选
            readOnly=False,       # 可选
        )
    ],
)
```

## 说明

- 当前实现仅支持**内联凭据**（`accessKeyId` / `accessKeySecret`）。
- Docker 运行时采用**按需挂载**（mount-or-reuse），不是预挂载所有 bucket。
- API/SDK 中 `ossfs.version` 字段存在，枚举为 `"1.0"` / `"2.0"`，省略时默认 `"2.0"`。
- Docker 运行时已按 `version` 区分挂载参数编码：
  - `1.0`：通过 `ossfs ... -o <option>` 挂载。
  - `2.0`：通过 `ossfs2 mount ... -c <config-file>` 挂载，`options` 以 `--<option>` 配置项写入配置文件。
- `options` 必须是**不带前缀 `-` 的原始参数值**（例如：`allow_other`、`umask=0022`）。

## 参考

- [OSEP-0003: Volume 与 VolumeBinding 支持](../../oseps/0003-volume-and-volumebinding-support.md)
- [Sandbox Lifecycle API 规范](../../specs/sandbox-lifecycle.yml)
