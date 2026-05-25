# OpenSandbox Code Interpreter 环境

中文 | [English](README.md)

这个目录包含了 Code Interpreter 沙箱的 Docker 构建文件。该镜像基于 `Ubuntu 24.04`
，并预装了多种主流编程语言及其多版本环境，旨在提供一个开箱即用的多语言代码执行环境。

## 特性

- **多语言支持**：预装 Python、Java、Node.js 和 Go 及其多个版本
- **版本切换**：无需重新构建，支持运行时快速切换版本
- **Jupyter 集成**：内置 Jupyter Notebook 并支持多语言内核
- **多架构支持**：同时支持 amd64 和 arm64 架构
- **clone3-workaround（仅 amd64）**：在 **linux/amd64** 镜像中安装 [AkihiroSuda/clone3-workaround](https://github.com/AkihiroSuda/clone3-workaround) v1.0.0 至 `/usr/local/bin/clone3-workaround`（上游无 arm64 预编译包），并安装 **`libseccomp2`**（上游二进制动态链接 `libseccomp`）。在极旧 Docker/containerd 宿主机上可用其包裹命令，例如 `clone3-workaround apt-get update`。
- **生产就绪**：针对容器化执行环境进行了优化

## 支持的语言与版本

镜像内预置了以下语言和版本：

| 语言          | 支持版本                          | 安装路径                   | 备注                     |
|:------------|:------------------------------|:-----------------------|:-----------------------|
| **Python**  | 3.10, 3.11, 3.12, 3.13, 3.14* | `/opt/python/versions` | 使用 `uv` 安装；3.14 为实验性版本 |
| **Java**    | 8, 11, 17, 21                 | `/usr/lib/jvm`         | OpenJDK; 含 Maven 3.9.2 |
| **Node.js** | v18, v20, v22                 | `/opt/node`            | 官方 Linux 二进制包          |
| **Go**      | 1.23, 1.24, 1.25              | `/opt/go`              | 官方 Linux 二进制包          |

*> 注意: 版本号可能会随构建时间更新至小版本的最新版。*

## 快速开始

### 1. 构建镜像

由于支持多架构（amd64/arm64），建议使用 Docker Buildx 构建：

```bash
# 进入目录
cd sandboxes/code-interpreter

# 构建本地镜像
docker build -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:latest .

# 多架构构建（需要 Docker Buildx）
docker buildx build --platform linux/amd64,linux/arm64 \
  -t sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:latest .
```

### 2. 运行容器

**指定自定义版本：**

```bash
docker run -it --rm \
  -e PYTHON_VERSION=3.11 \
  -e JAVA_VERSION=17 \
  -e NODE_VERSION=20 \
  -e GO_VERSION=1.24 \
  sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:latest
```

### `EXECD_CLONE3_COMPAT`（clone3-workaround）

若将 `EXECD_CLONE3_COMPAT` 设为 `1`、`true`、`yes`、`on` 或 `reexec`（与 [execd](../../components/execd/README_zh.md#沙箱内的-linux-clone3-兼容) 一致），入口脚本会在启动 Jupyter/内核前用 **`/usr/local/bin/clone3-workaround` 重新 `exec` 自身**。**linux/amd64** 镜像内含该二进制；**arm64** 构建会打印警告并跳过包装。包装成功后脚本会在当前进程树中 **`unset` `EXECD_CLONE3_COMPAT`**。设为 `0`、`false`、`off`、`no` 或不设置则关闭此逻辑。

## 如何切换版本

镜像内置了一个环境切换脚本 `/opt/opensandbox/code-interpreter-env.sh`，你需要使用 `source` 命令加载它来修改当前 Shell
的环境变量。

### 基本用法

```bash
source /opt/opensandbox/code-interpreter-env.sh <language> <version>
```

### 示例

**切换 Python 版本：**

```bash
# 切换到 Python 3.11
source /opt/opensandbox/code-interpreter-env.sh python 3.11
python3 --version
# Output: Python 3.11.x
```

**切换 Java 版本：**

```bash
# 切换到 Java 8
source /opt/opensandbox/code-interpreter-env.sh java 8
java -version
```

**切换 Node.js 版本：**

```bash
# 切换到 Node 22
source /opt/opensandbox/code-interpreter-env.sh node 22
node -v
```

**切换 Go 版本：**

```bash
# 切换到 Go 1.25
source /opt/opensandbox/code-interpreter-env.sh go 1.25
go version
```

### 查看可用版本

如果不指定版本号，脚本会列出当前镜像内已安装的可用版本：

```bash
# 查看所有 Python 版本
source /opt/opensandbox/code-interpreter-env.sh python

# 查看所有 Java 版本
source /opt/opensandbox/code-interpreter-env.sh java

# 查看所有 Node.js 版本
source /opt/opensandbox/code-interpreter-env.sh node

# 查看所有 Go 版本
source /opt/opensandbox/code-interpreter-env.sh go
```

## 默认版本

容器启动时的默认版本配置如下：

- **Python**: 3.14
- **Java**: 21
- **Node.js**: 22
- **Go**: 1.25

如需在 Dockerfile 层面永久修改默认版本，请调整 Dockerfile 底部的 `ENV PATH` 设置。

## Jupyter Notebook 集成

### 可用内核

镜像预装了所有支持语言的 Jupyter 内核：

- **Python**：所有 Python 版本的 ipykernel
- **Java**：IJava 内核
- **TypeScript/JavaScript**：tslab 内核
- **Go**：gonb 内核
- **Bash**：bash_kernel

### 启动 Jupyter

```bash
/opt/opensandbox/code-interpreter.sh
```

### 环境变量

- `JUPYTER_HOST`：Jupyter 服务器地址（默认：`http://127.0.0.1:44771`）
- `JUPYTER_PORT`：Jupyter 服务器端口（默认：`44771`）
- `JUPYTER_TOKEN`：访问令牌（默认：`opensandboxcodeinterpreterjupyter`）

## 高级用法

### 持久化工作空间

挂载本地目录以持久化您的工作：

```bash
docker run -it --rm \
  -v $(pwd)/workspace:/workspace \
  sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:latest
```

### 自定义配置

覆盖 Jupyter 配置：

```bash
docker run -it --rm \
  -v $(pwd)/jupyter_config.py:/root/.jupyter/jupyter_notebook_config.py \
  sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:latest
```

### 安装额外的包

**Python：**

```bash
python3 -m pip install pandas numpy --break-system-packages
```

**Node.js：**

```bash
npm install -g typescript
```

**Go：**

```bash
go install github.com/user/package@latest
```

**Java：**

```bash
mvn install dependency:copy-dependencies
```

## 架构说明

```
code-interpreter/
├── Dockerfile                          # 镜像Dockerfile
├── Dockerfile_base                     # 基础镜像Dockerfile
├── README.md                           # 英文文档
├── README_zh.md                        # 本文件
└── scripts/
    ├── code-interpreter-env.sh         # 版本切换脚本
    ├── code-interpreter.sh             # Jupyter 启动脚本
    └── jupyter_notebook_config.py      # Jupyter 配置文件
```

## 许可证

此项目是 OpenSandbox 套件的一部分。详情请参阅主 [LICENSE](../../LICENSE) 文件。

## 支持

问题和疑问：

- GitHub Issues: [OpenSandbox Issues](https://github.com/alibaba/OpenSandbox/issues)

## 相关项目

- [OpenSandbox](../../) - 主项目
- [Server](../../server/) - 服务器实现
- [Execd](../../components/execd/) - 运行时执行引擎
