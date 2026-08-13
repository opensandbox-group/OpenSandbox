---
title: 远程 Kubernetes subPath 初始化 POC
description: 使用隔离随机命名空间，通过 OpenSandbox 生命周期 API 验证 ensureSubPathDirectory 的安全演练手册。
---

# 远程 Kubernetes `ensureSubPathDirectory` POC

本手册使用 [`scripts/remote-k8s-subpath-poc.sh`](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/remote-k8s-subpath-poc.sh) 验证现有 `ensureSubPathDirectory` 功能。脚本默认只打印计划，只有显式确认后才会产生远程写操作。

::: warning
该脚本**不会**构建或推送镜像，不会读取 Secret 内容，不会使用 `kubectl apply`，也不会使用端口转发。新 sandbox 仅通过 OpenSandbox 生命周期 API `POST /v1/sandboxes` 创建；不会直接创建 BatchSandbox 或 Pod。
:::

## 覆盖范围和安全边界

一次已确认的运行只处理下列对象：

| 对象 | 名称或选择方式 | 创建方式 | 清理方式 |
| --- | --- | --- | --- |
| Namespace | `opensandbox-subpath-poc-<16 位小写十六进制>` | 父级编排在获批凭证下预置 | 本脚本绝不删除 |
| ServiceAccount | `opensandbox-subpath-poc-sa-<同一随机值>` | 父级编排在获批凭证下预置 | 本脚本绝不删除 |
| imagePullSecret | `opensandbox-subpath-poc-pull-<同一随机值>` | 父级编排在获批凭证下预置 | 本脚本只读取 metadata，绝不读取 data，也绝不删除 |
| PVC | `opensandbox-subpath-poc-pvc-<同一随机值>` | 生命周期 API 请求中的 PVC 自动创建 | 按精确名称删除 |
| BatchSandbox | 生命周期 API 返回的 UUID | 生命周期 API | 对该 UUID 调用生命周期 API 删除 |
| Pod | 控制器生成；仅以 `opensandbox.io/id=<返回 UUID>` 查询 | BatchSandbox 控制器 | 随精确 Namespace 清理 |

OpenSandbox 当前的生命周期 API 由服务端生成 UUID sandbox ID，因此 BatchSandbox/Pod 名称不能由客户端改为 POC 前缀。脚本不猜测或伪造该名称：它只接受 API 返回的精确 UUID，并且所有 Kubernetes 查询都局限在 POC Namespace。客户端明确命名的 Namespace、ServiceAccount、Secret、PVC、volume、subPath 和 metadata 均使用 POC 前缀。

脚本拒绝：

- `sandbox-tenant-a`；
- 不符合精确随机格式的 Namespace；
- 不存在的预置 Namespace、ServiceAccount、imagePullSecret，目标 PVC 已存在，或目标 Namespace 中已有 BatchSandbox；
- `http(s)://...:18080` 与 `http(s)://...:18090`，即正式服务端口；
- 非 loopback 的 lifecycle URL、未设置 `KUBECONFIG`，或者本地 Server 配置与 POC Namespace/BatchSandbox template mode/固定 digest 的 execd 镜像不一致。

清理函数由 `EXIT` trap 调用，只会删除 API 返回的单个 sandbox 与本次精确 PVC；不使用 `--all`、宽泛 label 删除或 Namespace 删除。Namespace、ServiceAccount 和 imagePullSecret 的创建与最终回收均归父级编排所有，避免脚本删除预置依赖或超出自身所有权的资源。

## 前置条件

1. 操作人员持有一个**单文件** `KUBECONFIG`，它可读取精确 POC Namespace、ServiceAccount、Secret metadata、BatchSandbox、PVC 与 Pod，并可在 POC Pod 内 `exec`；还必须能够删除本次精确 PVC。脚本不读取任何 Secret data。
2. 父级编排必须使用独立、已获批的凭证预置新的 POC Namespace、同随机值的 ServiceAccount 和 imagePullSecret。使用 [`scripts/remote-k8s-subpath-poc-provision.sh`](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/remote-k8s-subpath-poc-provision.sh)；它只能创建这三个精确资源，不能创建 Role/RoleBinding/ClusterRole、控制器、CRD、PVC、BatchSandbox、Pod 或 Service。
3. OpenSandbox Server 必须是**由操作人员外部提供的隔离本地进程**，仅针对该 POC Namespace 运行。`REMOTE_POC_BASE_URL` 只能指向 `127.0.0.1`、`localhost` 或 `::1`，且不能使用正式端口。脚本通过操作人员提供的 `REMOTE_POC_SERVER_CONFIG` 读取本地配置，确认 `[kubernetes].namespace` 精确等于 `POC_NAMESPACE`、`workload_provider = "batchsandbox"`、`runtime.type = "kubernetes"`、`runtime.execd_image` 是固定 `@sha256` digest，且 template 中仅配置一个匹配的 POC ServiceAccount。
4. `runtime.execd_image` 必须已包含 `/opensandbox-subpath-initializer`；这是现有功能的运行时前提。脚本只能验证固定 digest，不能检查镜像内容、构建或推送镜像。
5. 已准备一个目标集群可拉取的、含 `/bin/sh` 的测试镜像。该镜像由预置的 POC ServiceAccount 引用的 imagePullSecret 授权拉取。脚本只验证 Secret 的精确名称、类型 `kubernetes.io/dockerconfigjson` 和 ServiceAccount 的引用关系，绝不读取 Secret data。
6. 必须显式设置 `REMOTE_POC_STORAGE_CLASS` 为目标集群中适用于 POC 的 StorageClass 名称。该变量仅写入生命周期请求中**自动创建的本次精确 POC PVC**的 `storageClass`，不会修改 Server 配置、既有 PVC 或任何其他集群资源。
7. 对 JuiceFS 或其他共享存储，只能使用脚本生成的**新随机 subPath**。绝不复用、删除、修复或清空共享后端路径。显式执行前，操作人员必须确认删除本次 PVC 和 POC Namespace 不会影响任何共享数据，并显式设置 `REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1`；无法确认时不得执行。
8. 必须显式设置 `REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS` 为 61 至 300 的整数，且至少应大于等于隔离 Server 的 `sandbox_create_timeout_seconds` 再加上必要余量。该变量只控制本地 `curl` 等待 `POST /v1/sandboxes` 响应的最长时间，不修改 Server、Kubernetes 或 sandbox 超时。
9. 如本地 Server 启用了认证，将 API key 只注入当前终端的 `REMOTE_POC_API_KEY`；不要写入命令历史、脚本、文档或仓库。

若隔离本地 Server、预置依赖或配置检查无法确认，必须停止。不要尝试直接创建 CR、修改共享服务配置、使用正式端口、或仅以环境标志让脚本猜测 Namespace 路由。

## 生成随机 Namespace

先生成并保留一个随机 Namespace；必须严格为 `opensandbox-subpath-poc-` 加 16 位小写十六进制。然后由父级编排以获批凭证完成预置，并启动使用该 Namespace 的隔离本地 Server：

```bash
export POC_NAMESPACE="opensandbox-subpath-poc-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
printf '%s\n' "$POC_NAMESPACE"
```

不要复用旧 Namespace。父级编排必须只为这个新 Namespace 预置以下资源：

```text
Namespace:       $POC_NAMESPACE
ServiceAccount:  opensandbox-subpath-poc-sa-${POC_NAMESPACE#opensandbox-subpath-poc-}
imagePullSecret: opensandbox-subpath-poc-pull-${POC_NAMESPACE#opensandbox-subpath-poc-}
```

ServiceAccount 必须通过 `imagePullSecrets` 引用该同名随机 Secret；Secret 类型必须为 `kubernetes.io/dockerconfigjson`。此预置属于父级编排职责。provisioner 仅从 `REMOTE_POC_DOCKER_AUTH_KEY` 指定的本地 Docker config 单一 `auths` 条目构造 Secret，绝不打印凭证、不读取任何既有 Kubernetes Secret，也不复制其他 registry auth；临时 credential 文件在 Secret 创建后用 `shred -u` 擦除。待 POC 完成且本地 Server 已停止后，父级编排可按下文的精确 cleanup 模式回收 Namespace。

## 父级预置与精确 cleanup

以下命令均以默认 dry-run 运行：不访问 Kubernetes API，也不读取本地 Docker credential。仅在父级已批准后才设置相应确认变量。

```bash
export KUBECONFIG=/absolute/path/to/poc-provisioner-kubeconfig
export REMOTE_POC_SERVICE_ACCOUNT="opensandbox-subpath-poc-sa-${POC_NAMESPACE#opensandbox-subpath-poc-}"
export REMOTE_POC_IMAGE_PULL_SECRET="opensandbox-subpath-poc-pull-${POC_NAMESPACE#opensandbox-subpath-poc-}"
export REMOTE_POC_DOCKER_CONFIG=/absolute/path/to/local/docker/config.json
export REMOTE_POC_DOCKER_AUTH_KEY=registry.example.internal

# 默认：只打印三个精确计划资源；无远程调用、无本地 credential 读取。
scripts/remote-k8s-subpath-poc-provision.sh

# 经父级审批后：只创建精确 Namespace、Secret 和 ServiceAccount。
export REMOTE_POC_PROVISION_CONFIRM=1
scripts/remote-k8s-subpath-poc-provision.sh
unset REMOTE_POC_PROVISION_CONFIRM
```

provisioner 在任何写入前检查 `get/create namespaces`、POC Namespace 内 `create secrets` 与 `create serviceaccounts` 权限，并拒绝已存在的 Namespace。它不会检查、读取或覆盖既有 Kubernetes Secret。

POC runner 已通过 lifecycle API 删除其精确 sandbox/PVC、且本地 Server 已停止之后，父级才可请求删除 Namespace：

```bash
export REMOTE_POC_PROVISION_MODE=cleanup
scripts/remote-k8s-subpath-poc-provision.sh

export REMOTE_POC_CLEANUP_CONFIRM=1
export REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1
scripts/remote-k8s-subpath-poc-provision.sh
unset REMOTE_POC_CLEANUP_CONFIRM REMOTE_POC_PROVISION_MODE REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM
```

cleanup 在删除 Namespace 前枚举所有可列出的 namespaced resource 类型。白名单仅包含 `serviceaccount/default`、精确 POC ServiceAccount、精确 POC imagePullSecret、旧版自动生成的 `secret/default-token-<5 位小写字母数字>` 与 `secret/<精确 POC ServiceAccount>-token-<5 位小写字母数字>`、`configmap/kube-root-ca.crt`、`configmap/tapp-env-config`，以及 `events` 和 `events.events.k8s.io` 两种 Event API 资源。Events 仅因这是精确的随机 POC Namespace 才允许；cleanup 不会按名称或内容放宽其他资源。它不会读取 Secret data，也不会变更或单独删除这两个 system-injected ConfigMap。只要仍有 PVC、BatchSandbox、Pod、Job、Role、Service、其他 Secret/ConfigMap/ServiceAccount 或任何其他对象，或因权限不足无法验证某个资源类型，cleanup 就会拒绝。只有所有 POC workload 资源已不存在且操作人员已确认 Namespace 删除不影响 JuiceFS 或其他共享数据时，才会执行唯一的 Namespace 删除调用：精确的 `kubectl delete namespace "$POC_NAMESPACE"`；不使用 `--all`。

## 默认 dry-run

在仓库根目录执行以下命令。该模式不向 Kubernetes API 或生命周期 API 发出请求；它会打印精确 Namespace、PVC、subPath 和生命周期请求体：

```bash
export KUBECONFIG=/absolute/path/to/poc-kubeconfig
export REMOTE_POC_BASE_URL=http://127.0.0.1:18180
export REMOTE_POC_SANDBOX_IMAGE=registry.example.internal/poc/shell-image:approved
export REMOTE_POC_SERVER_CONFIG=/absolute/path/to/isolated-poc-server.toml
export REMOTE_POC_LOCAL_SERVER_CONFIRM=1
export REMOTE_POC_SERVICE_ACCOUNT="opensandbox-subpath-poc-sa-${POC_NAMESPACE#opensandbox-subpath-poc-}"
export REMOTE_POC_IMAGE_PULL_SECRET="opensandbox-subpath-poc-pull-${POC_NAMESPACE#opensandbox-subpath-poc-}"
export REMOTE_POC_STORAGE_CLASS=approved-poc-storage-class
export REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS=90

scripts/remote-k8s-subpath-poc.sh
```

若认证需要 API key，在运行前额外设置（不要回显该变量）：

```bash
export REMOTE_POC_API_KEY='由安全凭证系统在当前终端注入'
```

`REMOTE_POC_LOCAL_SERVER_CONFIRM=1` 不是 Namespace 绑定声明。它仅确认操作人员已经核实 loopback URL 对应的进程就是 `REMOTE_POC_SERVER_CONFIG` 指定的隔离本地 Server。脚本仍会解析该配置并验证精确 Namespace、BatchSandbox template mode、固定 execd digest 与 template 的 ServiceAccount；任何不匹配都会失败关闭。

## 显式执行和验证

仅当 dry-run 输出、服务端 Namespace 路由和测试镜像均已由操作人员审核后，再设置第二个确认开关：

```bash
export REMOTE_POC_CONFIRM=1
export REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1
scripts/remote-k8s-subpath-poc.sh
```

执行阶段依次：

1. 在本地静态检查 Server 配置、Namespace 前缀、ServiceAccount/Secret 的精确 POC 名称和 loopback URL；dry-run 不执行远程读取；
2. 显式执行时，读取预置的精确 Namespace、ServiceAccount 和 Secret metadata，确认 ServiceAccount 引用精确 imagePullSecret；不读取 Secret data；
3. 确认精确 PVC 不存在且 POC Namespace 中没有 BatchSandbox；
4. 调用 `POST /v1/sandboxes`，其中 volume 使用 PVC、可写的规范化相对 `subPath`，以及 `ensureSubPathDirectory: true`；
5. 使用 API 返回的 sandbox UUID 读取同 Namespace 的 BatchSandbox，验证它引用精确 PVC、主容器使用精确 subPath，且存在 `volume-subpath-initializer`；
6. 等待唯一带 `opensandbox.io/id=<UUID>` 的 Pod Ready，并在 `sandbox` 容器内写入、读回 subPath 下的 POC proof 文件；
7. 无论成功或失败，`finally`/`EXIT` 仅删除本次精确 sandbox 和 PVC；前提是操作人员已确认该 PVC 的删除不会影响 JuiceFS 或其他共享数据。它绝不删除、修复或清空共享后端路径。Namespace、ServiceAccount、Secret 保留给父级 provisioner 的显式 cleanup 模式回收。

成功时会显示：

```text
Verified exact PVC mount and volume-subpath-initializer in BatchSandbox.
PASS: the initializer-created subPath is mounted read-write in the sandbox.
```

## 失败诊断

| 现象 | 处理方式 |
| --- | --- |
| 脚本在 `REMOTE_POC_SERVER_CONFIG` 或 `REMOTE_POC_LOCAL_SERVER_CONFIRM` 处退出 | 停止执行。确认本地 loopback URL 的进程确实使用该配置，且配置中的 Namespace、BatchSandbox template mode、固定 execd digest 和 ServiceAccount 全部精确匹配。环境标志不能替代配置检查。 |
| 缺少 ServiceAccount 或 imagePullSecret | 由父级编排使用获批凭证在**新的精确 POC Namespace**中预置；确认 ServiceAccount `imagePullSecrets` 引用同一随机值的 Secret。不要读取、复制或在脚本中写入 Secret data。 |
| 创建返回非 202 | 查看 POC Server 的审计/应用日志，不要打印或读取 Secret。确认认证、Kubernetes runtime、BatchSandbox template mode 和 `runtime.execd_image`。 |
| 找不到 `volume-subpath-initializer` | 目标 Server/execd 镜像没有部署该功能；停止 POC，不要直接修改 BatchSandbox。 |
| Pod 未 Ready | 在**精确 POC Namespace**读取 BatchSandbox、Pod 状态和 Events；确认 PVC StorageClass、镜像可拉取性和控制器状态。 |
| cleanup 警告 | 记录精确 Namespace、PVC 和 API 返回的 UUID，使用同一受限 KUBECONFIG 只检查这些精确名称；禁止 `kubectl delete --all`。在 runner cleanup 完成、Server 停止后，使用父级 provisioner 的显式 cleanup 模式；它会拒绝任何残留的非预置 POC 对象。 |

脚本不会尝试弥补未知部署细节。服务端路由、初始化镜像或受限权限不满足时，应修复/确认这些前置条件后再重新生成新的随机 Namespace 执行。
