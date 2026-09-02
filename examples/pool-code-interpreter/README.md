# Pool + Code Interpreter

End-to-end demonstration of using a **pre-warmed sandbox pool** with the
Code Interpreter.  The script manages the full pool lifecycle through the
`PoolManager` API and then runs multi-language code snippets concurrently
across pool-backed sandboxes.

## What the demo does

| Phase | Description |
|---|---|
| **1 – Pool management** | Create a pool, list all pools, wait for warm buffer, optionally scale capacity |
| **2 – Concurrent execution** | Allocate N sandboxes from the pool and run Python / TypeScript / Go snippets in parallel |
| **3 – Cleanup** | Kill sandboxes, delete the pool |

## Prerequisites

- Kubernetes-backed OpenSandbox server (`runtime.type = kubernetes`)
- The OpenSandbox operator installed in your cluster
- `code-interpreter` image accessible from the cluster

## Setup

### 1. Pull the image

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.1
# Docker Hub mirror:
# docker pull opensandbox/code-interpreter:v1.0.1
```

### 2. Start the Kubernetes-backed server

```shell
uv pip install opensandbox-server

# Initialise config for k8s runtime
opensandbox-server init-config ~/.sandbox.toml --example k8s
curl -o ~/batchsandbox-template.yaml \
  https://raw.githubusercontent.com/alibaba/OpenSandbox/main/server/example.batchsandbox-template.yaml

opensandbox-server
```

### 3. Install Python packages

```shell
uv pip install opensandbox opensandbox-code-interpreter
```

## Run

```shell
# Minimal – uses all defaults
uv run python examples/pool-code-interpreter/main.py

# With explicit config
SANDBOX_DOMAIN=your-server:8080 \
SANDBOX_API_KEY=your-key \
POOL_NAME=ci-pool \
CONCURRENCY=5 \
uv run python examples/pool-code-interpreter/main.py

# Keep the pool alive after the demo (for repeated runs)
SKIP_POOL_DELETE=1 uv run python examples/pool-code-interpreter/main.py
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_DOMAIN` | `localhost:8080` | OpenSandbox server address |
| `SANDBOX_API_KEY` | _(none)_ | API key if authentication is enabled |
| `SANDBOX_IMAGE` | `code-interpreter:v1.0.1` | Container image for pool pods |
| `POOL_NAME` | `ci-pool` | Name of the pool to create or reuse |
| `CONCURRENCY` | `3` | Number of parallel sandboxes to allocate |
| `SKIP_POOL_DELETE` | `0` | Set to `1` to keep the pool after the demo |

## Pool CRD reference

The pool created by this demo maps to the following Kubernetes resource:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: ci-pool
  namespace: opensandbox
spec:
  template:
    metadata:
      labels:
        app: ci-pool
    spec:
      volumes:
        - name: sandbox-storage
          emptyDir: {}
        - name: opensandbox-bin
          emptyDir: {}
      initContainers:
        - name: task-executor-installer
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/task-executor:v0.1.0
          command: ["/bin/sh", "-c"]
          args:
            - cp /workspace/server /opt/opensandbox/bin/task-executor &&
              chmod +x /opt/opensandbox/bin/task-executor
          volumeMounts:
            - name: opensandbox-bin
              mountPath: /opt/opensandbox/bin
        - name: execd-installer
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.6
          command: ["/bin/sh", "-c"]
          args:
            - cp ./execd /opt/opensandbox/bin/execd &&
              cp ./bootstrap.sh /opt/opensandbox/bin/bootstrap.sh &&
              chmod +x /opt/opensandbox/bin/execd &&
              chmod +x /opt/opensandbox/bin/bootstrap.sh
          volumeMounts:
            - name: opensandbox-bin
              mountPath: /opt/opensandbox/bin
      containers:
        - name: sandbox
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.1
          command: ["/bin/sh", "-c"]
          args:
            - /opt/opensandbox/bin/task-executor -listen-addr=0.0.0.0:5758
              >/tmp/task-executor.log 2>&1
          env:
            - name: SANDBOX_MAIN_CONTAINER
              value: main
            - name: EXECD_ENVS
              value: /opt/opensandbox/.env
            - name: EXECD
              value: /opt/opensandbox/bin/execd
          volumeMounts:
            - name: sandbox-storage
              mountPath: /var/lib/sandbox
            - name: opensandbox-bin
              mountPath: /opt/opensandbox/bin
      tolerations:
        - operator: Exists
  capacitySpec:
    bufferMax: 3
    bufferMin: 1
    poolMax: 5
    poolMin: 0
```

## Example output

```text
============================================================
  Phase 1 – Pool management
============================================================
  Created pool 'ci-pool'
  capacity  bufferMax=3  bufferMin=1  poolMax=5  poolMin=0

  Pools in namespace (1 total):
    • ci-pool                 bufferMax=3  available=0

  Waiting for warm buffer in 'ci-pool' …
  status  total=1  allocated=0  available=0
  status  total=2  allocated=0  available=1
  Buffer ready – 1 warm pod(s) available.

============================================================
  Phase 2 – 3 concurrent sandboxes from pool 'ci-pool'
============================================================
  Allocating sandboxes …
  Allocated 3 sandbox(es) in 0.84s

  [task 1][py ] [1] Python 3.11.9 on linux
  [task 1][py ] result = 2
  [task 2][py ] [2] Python 3.11.9 on linux
  [task 2][py ] result = 4
  [task 3][py ] [3] Python 3.11.9 on linux
  [task 3][py ] result = 8
  [task 1][ts ] [1] TS sum(1..1) = 1
  [task 2][ts ] [2] TS sum(1..2) = 3
  [task 3][ts ] [3] TS sum(1..3) = 6
  [task 1][go ] [1] Go sum(1..1) = 1
  [task 2][go ] [2] Go sum(1..2) = 3
  [task 3][go ] [3] Go sum(1..3) = 6
  [task 1] finished in 3.21s
  [task 2] finished in 3.45s
  [task 3] finished in 3.38s

  Per-task times: ['3.21s', '3.45s', '3.38s']
  Total wall time (allocate + execute): 4.29s

============================================================
  Phase 3 – Cleanup
============================================================
  Killing sandboxes …
  3 sandbox(es) terminated.
  Deleted pool 'ci-pool'.

Demo complete.
```
