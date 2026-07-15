---
title: Execd Path Migration
description: Migration guide for the execd install path change from /opt/opensandbox/bin/ to /opt/opensandbox/.
---

# Execd Path Migration Guide

PR: [#1014](https://github.com/opensandbox-group/OpenSandbox/pull/1014)

## Background

OpenSandbox previously installed execd and bootstrap.sh to `/opt/opensandbox/bin/`, creating
unnecessary nesting. The execd image already places these artifacts at the root:

```
/execd
/bootstrap.sh
```

While bootstrap.sh defaults to `EXECD=/opt/opensandbox/execd` (no `bin/`), the Kubernetes
provider and Pool CR templates historically injected files into a `bin/` subdirectory. This
PR flattens the layout:

| Before | After |
|---|---|
| `/opt/opensandbox/bin/execd` | `/opt/opensandbox/execd` |
| `/opt/opensandbox/bin/bootstrap.sh` | `/opt/opensandbox/bootstrap.sh` |
| `/opt/opensandbox/bin/task-executor` | `/opt/opensandbox/task-executor` |

Additionally, the Docker runtime now uses the **full** `bootstrap.sh` from the execd image
instead of a minimal 15-line inline-generated shim. This gives Docker sandboxes the same
capabilities as Kubernetes: MITM CA trust setup, SIGTERM forwarding, pre-script sourcing,
and chained commands.

## Who is affected

| User profile | Impact |
|---|---|
| **code-interpreter image `<=v1.0.2`** | **Must upgrade to `v1.1.0`** — emptyDir at `/opt/opensandbox` shadows old scripts under `/opt/opensandbox/` |
| **Pool CR with custom templates** | Must update execd init container paths and volume mount |
| **Custom execd images** | Must now include `/bootstrap.sh` alongside `/execd` |
| **Docker runtime users (default)** | No manual action — bootstrap.sh is auto-installed |

## Migration steps

### 1. code-interpreter image

**Must upgrade the image from `v1.0.2` to `v1.1.0`.**

In code-interpreter `v1.0.2`, the entrypoint scripts (`code-interpreter.sh`,
`code-interpreter-env.sh`) live directly under `/opt/opensandbox/`. Before this PR,
the emptyDir volume was mounted at `/opt/opensandbox/bin/`, shadowing only that
subdirectory. Now the emptyDir mounts at `/opt/opensandbox/`, shadowing the entire
directory — including those scripts. The pod starts but cannot find its own entrypoint.

[PR #1012](https://github.com/opensandbox-group/OpenSandbox/pull/1012) moved all
code-interpreter scripts from `/opt/opensandbox/` to `/opt/code-interpreter/`,
avoiding the conflict. Version `v1.1.0` is the first image with that change.

```yaml
# In your Pool CR or sandbox creation request:
image: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0
```

The Python SDK (`opensandbox-code-interpreter`) does not require an upgrade — it
communicates with execd over the network and does not reference filesystem paths.

### 2. Pool CR template

If you maintain a custom Pool CR that defines the execd init container, update it:

```yaml
# Before
initContainers:
  - name: execd-installer
    image: opensandbox/execd:latest
    args:
      - |
        cp ./execd /opt/opensandbox/bin/execd &&
        cp ./bootstrap.sh /opt/opensandbox/bin/bootstrap.sh &&
        chmod +x /opt/opensandbox/bin/execd &&
        chmod +x /opt/opensandbox/bin/bootstrap.sh
    volumeMounts:
      - name: opensandbox-bin
        mountPath: /opt/opensandbox/bin

# After
initContainers:
  - name: execd-installer
    image: opensandbox/execd:latest
    args:
      - |
        cp ./execd /opt/opensandbox/execd &&
        cp ./bootstrap.sh /opt/opensandbox/bootstrap.sh &&
        chmod +x /opt/opensandbox/execd &&
        chmod +x /opt/opensandbox/bootstrap.sh
    volumeMounts:
      - name: opensandbox-bin
        mountPath: /opt/opensandbox
```

The same update applies to the task-executor init container if used:

```yaml
# Before
cp /workspace/server /opt/opensandbox/bin/task-executor && chmod +x /opt/opensandbox/bin/task-executor
# After
cp /workspace/server /opt/opensandbox/task-executor && chmod +x /opt/opensandbox/task-executor
```

And the main container's env vars and volume mount:

```yaml
# Before
- name: EXECD
  value: /opt/opensandbox/bin/execd
volumeMounts:
  - name: opensandbox-bin
    mountPath: /opt/opensandbox/bin

# After
- name: EXECD
  value: /opt/opensandbox/execd
volumeMounts:
  - name: opensandbox-bin
    mountPath: /opt/opensandbox
```

### 3. Custom execd images (Docker)

If you override `[runtime].execd_image` with a custom image, that image must now
contain `/bootstrap.sh` **in addition to** `/execd`. Previously only `/execd` was
required because the Docker runtime generated a minimal inline bootstrap script.

If your custom execd image is built from a Dockerfile that only copies the execd
binary, add bootstrap.sh:

```dockerfile
COPY bootstrap.sh /bootstrap.sh
```

The official `opensandbox/execd` image already includes both files and requires
no change.

### 4. Custom entrypoint scripts

If you hardcode `/opt/opensandbox/bin/execd` or `/opt/opensandbox/bin/bootstrap.sh`
in entrypoint scripts or build pipelines, update those references to `/opt/opensandbox/`.

## Compatibility

### Old paths are not available

After this change, the paths `/opt/opensandbox/bin/execd` and
`/opt/opensandbox/bin/bootstrap.sh` no longer exist. Any script, tool, or
template referencing them will fail with "file not found".

### How failures manifest

| Scenario | Failure mode |
|---|---|
| Old code-interpreter image (`<=v1.0.2`) | emptyDir at `/opt/opensandbox` shadows `code-interpreter.sh`; pod starts but entrypoint not found |
| Old Pool CR template | execd init container succeeds (it copies to its own mount), but main container cannot find `/opt/opensandbox/bin/bootstrap.sh` |
| Old custom execd image (no bootstrap.sh) | Docker sandbox creation fails with "bootstrap.sh not found in execd image cache" |

### Verification

After migration, confirm the sandbox starts successfully:

```bash
# In a running sandbox:
ls /opt/opensandbox/execd /opt/opensandbox/bootstrap.sh
# Both should exist. Verify /opt/opensandbox/bin/ does NOT exist:
test -d /opt/opensandbox/bin && echo "STALE PATH EXISTS" || echo "OK"
```

## Release notes

For release note authors: include a summary that links to this document.

```markdown
### Breaking: execd install path flattened

execd and bootstrap.sh are now installed to `/opt/opensandbox/` instead of
`/opt/opensandbox/bin/`. code-interpreter image users must upgrade to `>=v1.1.0`.
Custom Pool CR templates and custom execd images require updates.

See [execd Path Migration Guide](/reference/execd-path-migration) for full details.
```
