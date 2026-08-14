---
title: Code Interpreter
description: Complete demonstration of running Python code using the Code Interpreter SDK with OpenSandbox.
---

# Code Interpreter Sandbox

Complete demonstration of running Python code using the Code Interpreter SDK.

## Getting Code Interpreter image

Pull the prebuilt image from a registry:

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

# use docker hub
# docker pull opensandbox/code-interpreter:v1.1.0
```

## Start OpenSandbox server [local]

Start the local OpenSandbox server:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and access the Code Interpreter Sandbox

```shell
# Install OpenSandbox packages
uv pip install opensandbox opensandbox-code-interpreter

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY)
uv run python examples/code-interpreter/main.py
```

The script creates a Sandbox + CodeInterpreter, runs a Python code snippet and prints stdout/result, then terminates the remote instance.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |

## Example output

```text
=== Python example ===
[Python stdout] Hello from Python!

[Python result] {'py': '3.14.2', 'sum': 4}

=== Java example ===
[Java stdout] Hello from Java!

[Java stdout] 2 + 3 = 5

[Java result] 5

=== Go example ===
[Go stdout] Hello from Go!
3 + 4 = 7


=== TypeScript example ===
[TypeScript stdout] Hello from TypeScript!

[TypeScript stdout] sum = 6
```

## Code Interpreter Sandbox from pool

### Start OpenSandbox server [k8s]

Install the k8s OpenSandbox operator, and create a pool:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  labels:
    app.kubernetes.io/name: sandbox-k8s
    app.kubernetes.io/managed-by: kustomize
  name: pool-sample
  namespace: opensandbox
spec:
  template:
    metadata:
      labels:
        app: example
    spec:
      volumes:
        - name: sandbox-storage
          emptyDir: { }
        - name: opensandbox-bin
          emptyDir: { }
      initContainers:
        - name: task-executor-installer
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/task-executor:v0.1.0
          command: [ "/bin/sh", "-c" ]
          args:
            - |
              cp /workspace/server /opt/opensandbox/task-executor && 
              chmod +x /opt/opensandbox/task-executor
          volumeMounts:
            - name: opensandbox-bin
              mountPath: /opt/opensandbox
        - name: execd-installer
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.22
          command: [ "/bin/sh", "-c" ]
          args:
            - |
              cp ./execd /opt/opensandbox/execd && 
              cp ./bootstrap.sh /opt/opensandbox/bootstrap.sh &&
              chmod +x /opt/opensandbox/execd &&
              chmod +x /opt/opensandbox/bootstrap.sh
          volumeMounts:
            - name: opensandbox-bin
              mountPath: /opt/opensandbox
      containers:
        - name: sandbox
          image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0
          command:
          - "/bin/sh"
          - "-c"
          - |
            /opt/opensandbox/task-executor \
              -listen-addr=0.0.0.0:5758 \
              -log-dir=/tmp
          env:
          - name: SANDBOX_MAIN_CONTAINER
            value: main
          - name: EXECD_ENVS
            value: /opt/opensandbox/.env
          - name: EXECD
            value: /opt/opensandbox/execd
          volumeMounts:
            - name: sandbox-storage
              mountPath: /var/lib/sandbox
            - name: opensandbox-bin
              mountPath: /opt/opensandbox
      tolerations:
        - operator: "Exists"
  capacitySpec:
    bufferMax: 3
    bufferMin: 1
    poolMax: 5
    poolMin: 0
```

#### How Pool entrypoint injection works

The lifecycle API allocates an already-running Pod from the Pool, so it does not replace that Pod's `command`, `args`, or `env`. When a create request supplies an `entrypoint` or environment variables, the server records them in `BatchSandbox.spec.taskTemplate`. The controller then sends the task to the allocated Pod's IP on port `5758`.

The Pool template must provide all parts of that execution path:

- Install and run task-executor, listening on `0.0.0.0:5758`. Set its
  `-log-dir` explicitly so the troubleshooting path is deterministic; the
  example writes `/tmp/task-executor.log`.
- Install execd and `bootstrap.sh` into the shared volume before the Pod starts.
- Keep `bootstrap.sh` at `/opt/opensandbox/bootstrap.sh`. The server-generated task invokes that exact path. The execd binary can use another path only when the task-executor environment sets `EXECD` accordingly.
- Start execd through `bootstrap.sh` after allocation so request-specific values such as `EXECD_ACCESS_TOKEN` are available. The example above leaves task-executor as the warm Pod's foreground process for this reason.

The Pod YAML continuing to show the Pool template is therefore expected. Inspect the `BatchSandbox` resource and task-executor instead:

```shell
# Confirm that the server injected the requested process and environment.
kubectl get batchsandbox <sandbox-name> -n <namespace> \
  -o jsonpath='{.spec.taskTemplate}{"\n"}'

# Find the allocated Pod. The annotation value contains a JSON `pods` array.
kubectl get batchsandbox <sandbox-name> -n <namespace> \
  -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/alloc-status}{"\n"}'

# Replace <pool-pod> with the first Pod name from that array.
kubectl exec <pool-pod> -n <namespace> -- \
  sh -c 'test -x /opt/opensandbox/task-executor && test -x /opt/opensandbox/bootstrap.sh'
kubectl exec <pool-pod> -n <namespace> -- \
  tail -n 100 /tmp/task-executor.log

# Check the executor health endpoint from a second terminal while this runs.
kubectl port-forward pod/<pool-pod> -n <namespace> 5758:5758
curl http://127.0.0.1:5758/health
curl http://127.0.0.1:5758/getTasks

# The lifecycle server uses <sandbox-name>-0 as the task name. Check the task's
# captured output (adjust the path if task-executor uses a custom data directory).
kubectl exec <pool-pod> -n <namespace> -- \
  sh -c 'tail -n 100 /var/lib/sandbox/tasks/<sandbox-name>-0/stdout.log; tail -n 100 /var/lib/sandbox/tasks/<sandbox-name>-0/stderr.log'

# Check controller logs for delivery failures between the controller and port 5758.
kubectl logs -n opensandbox-system -l control-plane=controller-manager --tail=100
```

If `taskTemplate` exists but the health check cannot reach port `5758`, verify that task-executor is installed and remains running. The generated task intentionally starts `bootstrap.sh` in the background, so its wrapper can report success even when `bootstrap.sh` is missing or the requested entrypoint later fails. Do not rely on `taskFailed` or `taskLastErrorMessage` alone for these failures; inspect the task's captured `stderr.log` and `stdout.log`, then verify the execd or application process directly.

Start the k8s OpenSandbox server:

```shell
uv pip install opensandbox-server

# replace with your k8s cluster config, kubeconfig etc.
opensandbox-server init-config ~/.sandbox.toml --example k8s
curl -o ~/batchsandbox-template.yaml https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/server/opensandbox_server/examples/example.batchsandbox-template.yaml

opensandbox-server
```

### Create and access the Code Interpreter Sandbox (pool)

```shell
# Install OpenSandbox packages
uv pip install opensandbox opensandbox-code-interpreter

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY)
uv run python examples/code-interpreter/main_use_pool.py
```

### Pool example output

```text
=== Verify Environment Variable ===
[ENV Check] TEST_ENV value: test

[ENV Result] 'test'

=== Java example ===
[Java stdout] Hello from Java!

[Java stdout] 2 + 3 = 5

[Java result] 5

=== Go example ===
[Go stdout] Hello from Go!
3 + 4 = 7


=== TypeScript example ===
[TypeScript stdout] Hello from TypeScript!

[TypeScript stdout] sum = 6
```

## References

- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/code-interpreter)
