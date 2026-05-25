# OpenSandbox Kubernetes 控制器

[English](README.md) | [中文](README-ZH.md)

OpenSandbox Kubernetes 控制器，通过自定义资源管理沙箱环境。它在 Kubernetes 集群中提供**自动化沙箱生命周期管理**、**资源池化以实现快速供应**、**批处理沙箱创建**和**可选的任务编排**功能。

## 关键特性

- **灵活的沙箱创建**：在池化和非池化沙箱创建模式之间选择
- **批处理和单个交付**：支持单个沙箱（用于真实用户交互）和批处理沙箱交付（用于高吞吐量智能体强化学习场景）
- **可选任务调度**：集成任务编排，支持可选的分片任务模板以实现异构任务分发和定制化沙箱交付（例如，进程注入）
- **资源池化**：维护预热的资源池以实现快速沙箱供应
- **暂停和恢复**：通过 rootfs 快照持久化沙箱文件系统状态，在会话之间释放集群资源
- **全面监控**：实时跟踪沙箱和任务状态

## 功能特性

### 批处理沙箱管理
BatchSandbox 自定义资源允许您创建和管理多个相同的沙箱环境。主要功能包括：
- **灵活的创建模式**：支持池化（使用资源池）和非池化沙箱创建
- **单个和批处理交付**：根据需要创建单个沙箱（replicas=1）或批处理沙箱（replicas=N）
- **可扩展的副本管理**：通过副本配置轻松控制沙箱实例数量
- **自动过期**：设置 TTL（生存时间）以自动清理过期沙箱
- **可选任务调度**：内置任务执行引擎，支持可选任务模板
- **详细状态报告**：关于副本、分配和任务状态的综合指标

### 资源池化
Pool 自定义资源维护一个预热的计算资源池，以实现快速沙箱供应：
- 可配置的缓冲区大小（最小和最大）以平衡资源可用性和成本
- 池容量限制以控制总体资源消耗
- 基于需求的自动资源分配和释放
- 实时状态监控，显示总数、已分配和可用资源

### Pod 驱逐
Pool 支持优雅的 Pod 驱逐，适用于节点维护或资源回收等场景：

**工作原理：**
- 用户通过给 Pod 打上 `pool.opensandbox.io/evict` 标签请求驱逐
- 控制器会跳过已分配给 BatchSandbox 的 Pod（保护使用中的工作负载）
- 空闲 Pod 将被删除，触发池补充容量
- 标记驱逐的 Pod 不会被分配给新的 BatchSandbox

**自定义驱逐行为：**
您可以通过以下方式实现自定义驱逐策略：
1. 在 Pool 上设置 `pool.opensandbox.io/eviction-handler` 标签选择您的处理器
2. 实现 `EvictionHandler` 接口，包含 `NeedsEviction()` 和 `Evict()` 方法
3. 在工厂函数中注册您的处理器

### 任务编排
集成的任务管理系统，在沙箱内执行自定义工作负载：
- **可选执行**：任务调度完全可选 - 可以在不带任务的情况下创建沙箱
- **基于进程的任务**：支持在沙箱环境中执行基于进程的任务
- **异构任务分发**：使用 shardTaskPatches 为批处理中的每个沙箱定制单独的任务

### 高级调度
智能资源管理功能：
- 最小和最大缓冲区设置，以确保资源可用性同时控制成本
- 池范围的容量限制，防止资源耗尽
- 基于需求的自动扩展

## 暂停和恢复（Rootfs 快照）

OpenSandbox 支持通过将容器根文件系统持久化为 OCI 镜像来实现 Kubernetes 沙箱的**暂停和恢复**功能。

```text
时间 ---------------------------------------------------------------->

沙箱生命周期：   [运行中]--[暂停中]--[已暂停]--[恢复中]--[运行中]
                         |                     |
                  提交 rootfs          重写模板镜像
                  推送到 registry      基于快照重建运行时
                  释放 Pod/池化分配
```

### 工作原理

1. **暂停**：服务器 patch `BatchSandbox.spec.pause=true`。控制器创建内部 `SandboxSnapshot`，在同一节点上启动 commit Job，提交容器 rootfs 并推送到配置的 OCI registry。快照就绪后，控制器将同一个 `BatchSandbox` 置为 `Paused`，并释放运行时 Pod / 池化分配。
2. **恢复**：服务器 patch `BatchSandbox.spec.pause=false`。控制器读取最新的 `SandboxSnapshot`，把 `BatchSandbox` 模板镜像重写为快照镜像 URI，重建运行时，并将沙箱恢复到 `Running`。公共 `sandboxId` 在暂停/恢复周期中保持稳定。

当前暂停/恢复仅支持 `BatchSandbox.spec.replicas=1`。OpenSandbox server 创建的 Kubernetes 沙箱会固定使用 `replicas: 1`；如果直接创建 `BatchSandbox` CR 并设置为其他副本数，控制器会在 pause 入口拒绝请求，因为内部 pause snapshot 只记录一个源 Pod 的容器镜像状态。

### SandboxSnapshot CRD

`SandboxSnapshot` CR 是暂停/恢复生命周期的核心资源：

| 字段 | 位置 | 描述 |
|-------|----------|-------------|
| `spec.sandboxName` | Spec | 同命名空间下目标 `BatchSandbox` 的名称 |
| `status.phase` | Status | `Pending` → `Committing` → `Succeed` / `Failed` |
| `status.conditions` | Status | 带有 reason 和 message 的 `Ready` / `Failed` 条件 |
| `status.containers` | Status | 每个容器已提交的镜像 URI |
| `status.sourcePodName` | Status | 控制器解析的 Pod 名称 |
| `status.sourceNodeName` | Status | commit Job 选择的节点 |

### 前置条件

1. **OCI Registry**：用于存储快照镜像的可访问容器 registry。
2. **Kubernetes Secrets**：用于推送和拉取访问的 Docker 配置 secrets。
3. **控制器配置**：为 controller manager 配置快照 registry 和 secret 参数。
4. **控制器 RBAC**：控制器需要 `secrets: get` 权限（已包含在 Helm chart 和 `make manifests` 输出中）。

### 控制器配置

快照控制器支持以下命令行参数：

| 参数 | 默认值 | 描述 |
|------|---------|-------------|
| `--snapshot-registry` | `""` | 快照镜像使用的 OCI registry 前缀 |
| `--snapshot-push-secret` | `""` | commit Job 推送快照时使用的 Secret 名称 |
| `--resume-pull-secret` | `""` | 恢复后沙箱拉取镜像时注入的 Secret 名称 |
| `--image-committer-image` | `image-committer:dev` | 用于 commit 操作的镜像（必须包含 `nerdctl` 工具） |
| `--commit-job-timeout` | `10m` | commit Job 的超时时间 |
| `--snapshot-registry-insecure` | `false` | 是否让快照 commit Job 使用 insecure registry 模式 |

这些参数在控制器启动时配置。`image-committer-image` 必须是受信任的、包含 `nerdctl` 的容器镜像，以执行 rootfs commit 和推送操作。Commit Job 会在源 Pod 所在节点挂载宿主机 containerd socket，因此该镜像实际拥有节点级 runtime 访问能力。生产环境建议使用 digest pinning，或通过镜像仓库/准入策略限制来源。

本地开发时，示例 manager manifest 会直接传入这些 registry 和 secret 参数：

```yaml
- --snapshot-registry=<your-registry>/sandboxes
- --snapshot-registry-insecure=true # 仅用于 HTTP 或自签名证书的本地 registry
- --snapshot-push-secret=registry-snapshot-push-secret
- --resume-pull-secret=registry-pull-secret
```

当前 Helm chart 已直接暴露 `controller.snapshot.*` 这组 values，包括 `imageCommitterImage`、`commitJobTimeout`、`registry`、`registryInsecure`、`snapshotPushSecret` 和 `resumePullSecret`。

**源码 / Kustomize 部署：**

如果通过源码执行 `make deploy` 部署，Makefile 目前只会改写 `CONTROLLER_IMG`。快照相关 flags 仍然来自 `config/manager/manager.yaml`（或您自己的 Kustomize overlay / patch）。如果需要修改 registry、secret 或 image-committer 设置，请先更新该 manifest，再执行：

```sh
make deploy CONTROLLER_IMG=<controller-image>
```

### 快速设置

```bash
# 创建推送 secret
kubectl create secret docker-registry registry-snapshot-push-secret \
  --docker-server=<your-registry> \
  --docker-username=<user> \
  --docker-password=<token>

# 创建拉取 secret（可以复用推送 secret）
kubectl create secret docker-registry registry-pull-secret \
  --docker-server=<your-registry> \
  --docker-username=<user> \
  --docker-password=<token>
```

然后为 controller manager 配置：

```yaml
- --snapshot-registry=<your-registry>/sandboxes
- --snapshot-registry-insecure=true # 仅用于 HTTP 或自签名证书的本地 registry
- --snapshot-push-secret=registry-snapshot-push-secret
- --resume-pull-secret=registry-pull-secret
```

快照镜像的保留策略由 registry 管理。删除 `SandboxSnapshot` 只会清理 Kubernetes commit/unpause Job，不会删除已经推送到 registry 的 OCI 镜像。请根据环境为 `snap-gen<N>` 这类标签配置 registry retention/GC。

### CRD 清理

卸载时删除 SandboxSnapshot CRD：

```bash
kubectl delete crd sandboxsnapshots.sandbox.opensandbox.io
```

有关包括故障排除和失败场景在内的完整指南，请参见 [`docs/pause-resume.md`](../docs/pause-resume.md)。

## 运行时 API 支持说明

- Kubernetes 运行时通过 rootfs 快照支持 `pause` / `resume` 生命周期 API。参见上面的[暂停和恢复](#暂停和恢复rootfs-快照)。
- Docker 运行时支持 cgroup 级别的冻结（`pause`/`resume`），但不会在重启之间持久化文件系统状态。


## 与 [kubernates-sigs/agent-sandbox](kubernates-sigs/agent-sandbox) 的关系

BatchSandbox 并非重复实现 Agent-Sandbox 的基础功能，而是作为其补充，提供了额外的增强能力：

1. **批量 Sandbox 语义**：在强化学习（RL）训练等场景下，显著提升 Sandbox 的交付吞吐量
2. **Task 调度能力**：通过 Task 调度实现差异化 Sandbox 交付，例如在交付 Sandbox 之前向容器内注入自定义进程

因此，您可以根据具体应用场景选择合适的项目作为 Sandbox 底层运行时。

### 性能测试

BatchSandbox 与 Sig Agent-Sandbox 在吞吐量方面的性能对比测试。

**测试环境**

**Controller 组件配置**
- 资源规格：request: 12C32G, limit: 16C64G
- 并发配置：
  - **Sig Agent-Sandbox**：共 3 个 controller（sandbox、sandboxclaim、sandboxwarmppool），代码中未提供并发度配置，默认值为 1
  - **BatchSandbox**：共 2 个 controller，batchsandbox controller 并发度为 32，pool controller 并发度为 1

**Pool 配置**
- 镜像：busybox:latest
- 资源规格：0.1C256MB

> **补充说明**：虽然 BatchSandbox 的 batchsandbox-controller 并发度为 32，但测试用例中仅创建了一个 BatchSandbox 对象，实际等价于并发度为 1。因此在并发度方面，BatchSandbox 与 SIG Agent-Sandbox 保持一致。

**性能对比结果**

在都使用资源池的情况下，交付 100 个 Sandbox 的总耗时对比：

| 测试场景 | 总耗时 (秒) |
|---------|------------|
| SIG Agent-Sandbox (创建并发=1) | 76.35 |
| SIG Agent-Sandbox (创建并发=10) | 23.17 |
| SIG Agent-Sandbox (创建并发=50) | 33.85 |
| BatchSandbox | 0.92 |

**原因分析**

核心差异：Sig Agent-Sandbox 和 BatchSandbox 批量交付 N 个 Sandbox 的时间复杂度分别为 O(N) 和 O(1)。

**Sig Agent-Sandbox 原理**
- 每个 Sandbox 的交付流程需要执行以下写操作（写操作总数与 Sandbox 规模成正比）：
  1. 创建一个 SandboxClaim
  2. 创建一个 Sandbox
  3. 更新 Pod 一次（从资源池中接管 Pod）
  4. 更新 Sandbox Status 一次
  5. 更新 SandboxClaim Status 一次

**BatchSandbox 原理**
- 每批 Sandbox 的交付流程需要执行以下写操作（写操作总数与 Sandbox 规模无关）：
  1. 创建一个 BatchSandbox
  2. 更新 BatchSandbox annotation 一次（写入批分配结果）
  3. 更新 BatchSandbox status 一次

## 入门指南

![](images/deploy-example.gif)

### 先决条件
- go 版本 v1.24.0+
- docker 版本 17.03+
- kubectl 版本 v1.11.3+
- 访问 Kubernetes v1.21.1+ 集群

如果您没有 Kubernetes 集群的访问权限，可以使用 [kind](https://kind.sigs.k8s.io/) 创建一个本地 Kubernetes 集群进行测试。Kind 在 Docker 容器中运行 Kubernetes 节点，使得设置本地开发环境变得容易。

安装 kind：
- 从[发布页面](https://github.com/kubernetes-sigs/kind/releases)下载适用于您操作系统的发布二进制文件并将其移动到 `$PATH` 中
- 或使用包管理器：
  - macOS (Homebrew)：`brew install kind`
  - Windows (winget)：`winget install Kubernetes.kind`

安装 kind 后，使用以下命令创建集群：
```sh
kind create cluster
```

此命令默认创建单节点集群。要与其交互，请使用生成的 kubeconfig 运行 `kubectl`。

**Kind 用户的重要说明**：如果您使用的是 kind 集群，在使用 `make docker-build` 构建镜像后，需要将控制器和任务执行器镜像加载到 kind 节点中。这是因为 kind 在 Docker 容器中运行 Kubernetes 节点，无法直接访问本地 Docker 守护进程中的镜像。

使用以下命令将镜像加载到 kind 集群中：
```sh
kind load docker-image <controller-image-name>:<tag>
kind load docker-image <task-executor-image-name>:<tag>
```

例如，如果您使用 `make docker-build CONTROLLER_IMG=my-controller:latest` 构建镜像，则使用以下命令加载：
```sh
kind load docker-image my-controller:latest
```

完成后使用以下命令删除集群：
```sh
kind delete cluster
```

有关使用 kind 的更多详细说明，请参阅[官方 kind 文档](https://kind.sigs.k8s.io/docs/user/quick-start/)。

### 部署

此项目需要两个独立的镜像 - 一个用于控制器，另一个用于任务执行器组件。

#### 方式 1：使用 Helm 部署（推荐）

**从 GitHub Release 安装：**

您可以直接从 GitHub Releases 安装 OpenSandbox Controller。查看 [Releases 页面](https://github.com/alibaba/OpenSandbox/releases?q=helm%2Fopensandbox-controller&expanded=true) 了解所有可用版本。

```sh
# 将 <version> 替换为所需版本（例如：0.1.0）
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/<version>/opensandbox-controller-<version>.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

具体版本示例：
```sh
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

您也可以先下载 chart 然后再安装：
```sh
# 下载 chart
wget https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/<version>/opensandbox-controller-<version>.tgz

# 从本地文件安装
helm install opensandbox-controller ./opensandbox-controller-<version>.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

**自定义安装：**

使用 `--set` 参数自定义配置：

```sh
# 示例：自定义资源限制
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace \
  --set controller.replicaCount=2 \
  --set controller.resources.limits.cpu=1000m \
  --set controller.resources.limits.memory=512Mi

# 示例：自定义日志级别
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace \
  --set controller.logLevel=debug
```

或使用 values 文件进行复杂配置：

```sh
# 创建自定义 values 文件
cat > custom-values.yaml <<EOF
controller:
  replicaCount: 2
  resources:
    limits:
      cpu: 1000m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
  logLevel: debug
EOF

# 使用自定义 values 安装
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace \
  -f custom-values.yaml
```

**从源码安装（用于开发）：**

如果您正在进行开发或需要自定义 chart：

1. **构建和推送您的镜像：**
   ```sh
   # 构建和推送控制器镜像
   make docker-build docker-push CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   
   # 构建和推送任务执行器镜像
   make docker-build-task-executor docker-push-task-executor TASK_EXECUTOR_IMG=<some-registry>/opensandbox-task-executor:tag
   ```

2. **使用 Helm 安装：**
   ```sh
   helm install opensandbox-controller ./charts/opensandbox-controller \
     --set controller.image.repository=<some-registry>/opensandbox-controller \
     --set controller.image.tag=<tag> \
     --namespace opensandbox-system \
     --create-namespace
   ```

**验证安装：**

检查控制器是否运行：
```sh
kubectl get pods -n opensandbox-system
kubectl get deployment -n opensandbox-system

# 查看日志
kubectl logs -n opensandbox-system -l control-plane=controller-manager -f
```

**升级：**

```sh
# 升级到新版本
helm upgrade opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/<new-version>/opensandbox-controller-<new-version>.tgz \
  --namespace opensandbox-system
```

**卸载：**

```sh
helm uninstall opensandbox-controller -n opensandbox-system
```

有关更多配置选项和高级用法，请参阅 [Helm Chart README](charts/opensandbox-controller/README.md)。

#### 方式 2：使用 Kustomize 部署

1. **构建和推送您的镜像：**
   ```sh
   # 构建和推送控制器镜像
   make docker-build docker-push CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   
   # 构建和推送任务执行器镜像
   make docker-build-task-executor docker-push-task-executor TASK_EXECUTOR_IMG=<some-registry>/opensandbox-task-executor:tag
   ```

   **注意：** 这些镜像应该发布在您指定的个人注册表中。需要能够从工作环境中拉取镜像。如果上述命令不起作用，请确保您对注册表具有适当的权限。

2. **将 CRD 安装到集群中：**
   ```sh
   make install
   ```

3. **将管理器部署到集群：**
   ```sh
   make deploy CONTROLLER_IMG=<some-registry>/opensandbox-controller:tag
   ```

   **注意**：`make deploy` 只会改写 controller 镜像。如果您的 Pool / BatchSandbox 模板会引用 `TASK_EXECUTOR_IMG`，请单独构建并推送该镜像。您也可能需要在执行这些命令前具备集群管理员权限。

**Kind 用户的重要说明**：如果您使用的是 kind 集群，需要在构建镜像后将两个镜像都加载到 kind 节点中：
```sh
kind load docker-image <controller-image-name>:<tag>
kind load docker-image <task-executor-image-name>:<tag>
```

### 创建 BatchSandbox 和 Pool 资源

#### 基础示例
创建一个简单的非池化沙箱，不带任务调度：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: basic-batch-sandbox
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: sandbox-container
        image: nginx:latest
        ports:
        - containerPort: 80
```

应用批处理沙箱配置：
```sh
kubectl apply -f basic-batch-sandbox.yaml
```

检查批处理沙箱状态：
```sh
kubectl get batchsandbox basic-batch-sandbox -o wide
```

示例输出：
```sh
NAME                   DESIRED   TOTAL   ALLOCATED   READY   EXPIRE   AGE
basic-batch-sandbox    2         2       2           2       <none>   5m
```

状态字段说明：
- **DESIRED**：请求的沙箱数量
- **TOTAL**：创建的沙箱总数
- **ALLOCATED**：成功分配的沙箱数量
- **READY**：准备使用的沙箱数量
- **EXPIRE**：过期时间（未设置时为空）
- **AGE**：资源创建以来的时间

沙箱准备好后，您可以在注解中找到端点信息：
```sh
kubectl get batchsandbox basic-batch-sandbox -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/endpoints}'
```

这将显示交付沙箱的 IP 地址。

#### 高级示例

##### 不带任务的池化沙箱
首先，创建一个资源池：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: example-pool
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: nginx:latest
        ports:
        - containerPort: 80
  capacitySpec:
    bufferMax: 10
    bufferMin: 2
    poolMax: 20
    poolMin: 5
```

应用资源池配置：
```sh
kubectl apply -f pool-example.yaml
```

**可选：配置扩容速率控制** - 添加 `scaleStrategy` 限制扩容节奏：
```yaml
  scaleStrategy:
    maxUnavailable: "20%"  # 或绝对数量如 5
```

该配置控制扩容过程中允许不可用的 Pod 数量。例如，当 `poolMax=50` 且 `maxUnavailable=20%` 时，每次最多扩容 10 个 Pod。

使用资源池创建一批沙箱：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: pooled-batch-sandbox
spec:
  replicas: 3
  poolRef: example-pool
```

应用批处理沙箱配置：
```sh
kubectl apply -f pooled-batch-sandbox.yaml
```

##### 带异构任务的池化沙箱
创建一批带有基于进程的异构任务的沙箱。为了使任务执行正常工作，任务执行器必须作为 sidecar 容器部署在资源池模板中，并与沙箱容器共享进程命名空间：

首先，创建一个带有任务执行器 sidecar 的资源池：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: task-example-pool
spec:
  template:
    spec:
      shareProcessNamespace: true
      containers:
      - name: sandbox-container
        image: ubuntu:latest
        command: ["sleep", "3600"]
      - name: task-executor
        image: <task-executor-image>:<tag>
        securityContext:
          capabilities:
            add: ["SYS_PTRACE"]
  capacitySpec:
    bufferMax: 10
    bufferMin: 2
    poolMax: 20
    poolMin: 5
```

使用我们刚刚创建的资源池创建一批带有基于进程的异构任务的沙箱：

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: task-batch-sandbox
spec:
  replicas: 2
  poolRef: task-example-pool
  taskTemplate:
    spec:
      process:
        command: ["echo", "Default task"]
  shardTaskPatches:
  - spec:
      process:
        command: ["echo", "Custom task for sandbox 1"]
  - spec:
      process:
        command: ["echo", "Custom task for sandbox 2"]
        args: ["with", "additional", "arguments"]
```

应用批处理沙箱配置：
```sh
kubectl apply -f task-batch-sandbox.yaml
```

检查带任务的批处理沙箱状态：
```sh
kubectl get batchsandbox task-batch-sandbox -o wide
```

示例输出：
```sh
NAME                   DESIRED   TOTAL   ALLOCATED   READY   TASK_RUNNING   TASK_SUCCEED   TASK_FAILED   TASK_UNKNOWN   EXPIRE   AGE
task-batch-sandbox     2         2       2           2       0              2              0             0              <none>   5m
```

任务状态字段说明：
- **TASK_RUNNING**：当前正在执行的任务数
- **TASK_SUCCEED**：成功完成的任务数
- **TASK_FAILED**：失败的任务数
- **TASK_UNKNOWN**：状态未知的任务数

当您删除带有运行任务的 BatchSandbox 时，控制器将首先停止所有任务，然后删除 BatchSandbox 资源。一旦所有任务都成功终止，BatchSandbox 将被完全删除，沙箱将返回到资源池中以供重用。

删除 BatchSandbox：
```sh
kubectl delete batchsandbox task-batch-sandbox
```

您可以通过观察 BatchSandbox 状态来监控删除过程：
```sh
kubectl get batchsandbox task-batch-sandbox -w
```

### 监控资源
检查资源池和批处理沙箱的状态：
```sh
# 查看资源池状态
kubectl get pools

# 查看批处理沙箱状态
kubectl get batchsandboxes

# 获取特定资源的详细信息
kubectl describe pool example-pool
kubectl describe batchsandbox example-batch-sandbox
```

## 项目结构

```
├── api/
│   └── v1alpha1/          # 自定义资源定义（BatchSandbox, Pool）
├── cmd/
│   ├── controller/         # 主控制器管理器入口点
│   └── task-executor/     # 任务执行器二进制文件
├── config/
│   ├── crd/               # 自定义资源定义清单
│   ├── default/           # 控制器部署的默认配置
│   ├── manager/           # 控制器管理器配置
│   ├── rbac/              # 基于角色的访问控制清单
│   └── samples/           # 资源的示例 YAML 清单
├── hack/                  # 开发脚本和工具
├── images/                # 文档图片
├── internal/
│   ├── controller/        # 核心控制器实现
│   ├── scheduler/         # 资源分配和调度逻辑
│   ├── task-executor/     # 任务执行引擎内部实现
│   └── utils/             # 实用函数和助手
├── pkg/
│   └── task-executor/     # 共享的任务执行器包
└── test/                  # 测试套件
```

## 贡献
欢迎为 OpenSandbox Kubernetes 控制器项目做出贡献。请随时提交问题、功能请求和拉取请求。

**注意：** 运行 `make help` 以获取所有潜在 `make` 目标的更多信息

更多信息请参见 [Kubebuilder 文档](https://book.kubebuilder.io/introduction.html)

## 许可证
此项目在 Apache 2.0 许可证下开源。

您可以将 OpenSandbox 用于个人或商业项目，只要遵守许可证条款即可。
