---
title: Docker-in-Docker on Kata Sandboxes
description: Run a nested Docker daemon inside a Kata-isolated OpenSandbox sandbox — privileged prerequisites, runtime type mapping, storage driver selection, cgroup delegation, and common pitfalls.
---

# Docker-in-Docker on Kata Sandboxes

Many workloads need their own Docker daemon inside the sandbox: building images, running `docker compose` stacks, or CI jobs that spawn helper containers. This guide covers how to run a nested daemon (Docker-in-Docker, DinD) inside a sandbox that is itself isolated by Kata Containers, and the pitfalls that account for most failures: the privileged prerequisite, storage-driver selection, and cgroup delegation.

For server-side secure runtime configuration and RuntimeClass setup, see [Secure Container Runtime](/guides/secure-container).

## Why Kata works well for nested Docker

Under Kata the sandbox runs as a full VM, so a nested `dockerd` runs against the guest kernel — no nested virtualization is involved. Compared with runtimes that cap the sandbox's UID/GID space with a user namespace, the guest kernel gives the nested daemon:

- **A complete kernel feature set**: full `iptables`/`nft` tables (including `nat`), so nested containers network normally.
- **An unrestricted UID/GID space**: the guest `uid_map` is complete, so build contexts and images with large numeric owners (common with network-mounted sources or Windows-packaged archives) are handled natively.
- **A real cgroup hierarchy**: `docker run --memory` / `--cpus` and `mem_limit`/`cpus` in `docker compose` are enforced by the guest kernel's cgroup v2 — provided delegation is configured (below).

## Prerequisites

- The sandbox image must contain the daemon and CLI (for example `docker:27-dind`, or a custom image with `dockerd` and `docker` installed).
- The server must be configured with a Kata secure runtime. The runtime type and RuntimeClass must be paired correctly:

  | `secure_runtime.type` | `k8s_runtime_class` | Container rootfs inside the guest |
  | --- | --- | --- |
  | `"kata"` | `"kata-qemu"` | shared into the guest over virtio-fs |
  | `"firecracker"` | `"kata-fc"` | devmapper block device (no virtio-fs; Kubernetes only) |

  `"kata-fc"` is only used with `secure_runtime.type = "firecracker"` — never with `"kata"` (see Example 3 in the [Secure Container Runtime](/guides/secure-container) guide). The two runtime classes also differ in storage behavior, which matters for the [storage driver](#storage-driver-give-var-lib-docker-a-filesystem-overlay2-can-use) section below.

- **Privileges: the sandbox pod must run privileged.** Rootful `dockerd` needs more than a plain container gets: it writes mounts and cgroup state under `/sys`, requires block-device and netns access for its bridge/NAT setup, and runs against seccomp/AppArmor profiles that do not expect a daemon in the container. Granting only capability lists such as `NET_ADMIN`/`SYS_ADMIN` is **not** sufficient — in a non-privileged container `/sys` and `/sys/fs/cgroup` are mounted read-only, block device nodes are not present, and the default seccomp/AppArmor profiles still block parts of what `dockerd` does. In practice DinD on Kubernetes runs with `privileged: true`.

- On Kata, pair `privileged: true` with `privileged_without_host_devices = true` on the node's containerd runtime handler for the Kata runtime class, so a privileged sandbox pod does not receive the node's host devices inside the guest (clusters installed with kata-deploy set this by default — verify your handler config).

- **Where the grant goes (Kubernetes / BatchSandbox)**: set `privileged: true` on the sandbox container's `securityContext` in the **BatchSandbox CR template file** configured at `[kubernetes].batchsandbox_template_file` — not a `SandboxTemplate` CRD (that CRD has no main-container pod spec). This is a partial BatchSandbox manifest, like the [shipped AKS example](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/aks-kata/batchsandbox-template-configmap.yaml); the server merges the template's `sandbox` container `securityContext` (or the first container's) into the runtime container, and template values win wherever the runtime does not set one:

  ```yaml
  # BatchSandbox CR template file ([kubernetes].batchsandbox_template_file)
  spec:
    template:
      spec:
        containers:
          - name: sandbox   # the server merges this container's securityContext into the runtime main container
            securityContext:
              privileged: true
  ```

  The target namespace must also run Pod Security Admission at the `privileged` enforce level (`pod-security.kubernetes.io/enforce=privileged`); `baseline` and `restricted` reject privileged containers and added capabilities.

  ::: warning Verification status
  This guide documents the standard DinD-on-Kubernetes pattern — a privileged sandbox pod running the official Docker `dind` image, paired with `privileged_without_host_devices = true` on the Kata runtime handler. It has not yet been verified end to end on an OpenSandbox + Kata deployment; capability, storage, and cgroup behavior vary with the Kata, containerd, and kernel versions in your cluster. Create a sandbox, run `docker info` inside, check the storage driver, and run a nested `docker run` before relying on this setup in production.
  :::

- **Docker mode (Docker provider) is not supported for DinD.** The server drops `NET_ADMIN` and `SYS_ADMIN` from sandbox containers by default (`[docker].drop_capabilities`) and exposes no capability-add option for them — removing the entries only restores the container engine's default capability set, which does not include `NET_ADMIN` either. Run nested-daemon workloads on the Kubernetes (BatchSandbox) path above.

## DinD and `network_policy` are mutually exclusive

When a sandbox request carries a `network_policy`, the server always adds `drop: [NET_ADMIN]` to the sandbox's main container so the egress sidecar stays the only writer of the pod's network rules. Kubernetes applies capability drops after adds, so a DinD template that grants capabilities cannot restore `NET_ADMIN` on a policy sandbox — the nested daemon's bridge/NAT setup fails, and DinD with default daemon networking is unsupported under a policy. Allow-listing registry endpoints (below) does not change this: it fixes name resolution and reachability, not the daemon's own bridge setup.

::: warning `privileged: true` bypasses the policy's capability drop
`privileged: true` in the BatchSandbox template file is carried into every pod created from that template, and a privileged container receives the full capability set regardless of `capabilities.drop` — including the `NET_ADMIN` drop that the policy relies on. A privileged template therefore silently defeats the egress isolation of every `network_policy` sandbox it serves, and the server does not currently detect or reject this combination. Never serve policy-governed sandboxes from the same template (or the same server deployment) as DinD sandboxes, and verify after creation that a policy sandbox's emitted pod is not privileged.
:::

A `dockerd --bridge=none --iptables=false` daemon starts without `NET_ADMIN`, but nested containers then share the sandbox's network namespace and get no isolated networks or port publishing — treat that as an untested escape hatch, not a supported configuration.

## Storage driver: give `/var/lib/docker` a filesystem `overlay2` can use

Under Kata, the sandbox container's root filesystem is not a local filesystem inside the guest VM: on `kata-qemu` the rootfs is shared into the guest over virtio-fs, and on `kata-fc` it is a devmapper block device. Neither can host `dockerd`'s `overlay2` data directory — overlay upper/work layers are not supported on a virtio-fs share, and the devmapper rootfs layout is not a filesystem the driver can build on. Started against the container rootfs, the daemon silently falls back to the `vfs` driver, which is significantly slower and uses far more disk.

The same limitation applies to any directory shared into the guest over virtio-fs — including a regular sandbox volume or PVC-backed mount — so "mount a volume" is not automatically a fix for the storage driver. What the daemon needs is a filesystem that lives inside the guest and supports `overlay2`.

For short-lived sandboxes, put `/var/lib/docker` on a `tmpfs` — with its size chosen to fit **inside the sandbox memory limit**: under Kata a tmpfs consumes guest VM memory and counts against the pod's memory limit, so an oversized `size=` (for example `32g` on a small sandbox) will OOM the sandbox long before the tmpfs fills.

For long-lived sandboxes with larger image caches, a block-device-backed filesystem would be the durable option, but OpenSandbox sandboxes currently have no supported way to expose a raw block device (raw block PVCs are not wired into the sandbox pod, and the sandbox volume API does not cover block devices), so this has to be provisioned by the platform image or bootstrap layer rather than from the sandbox API.

The complete bootstrap, in the required order (storage mount and cgroup delegation both before daemon startup), is in the [next section](#cgroup-delegation-make-memory-and-cpus-enforce).

## cgroup delegation: make `--memory` and `--cpus` enforce

The nested daemon creates cgroups for its containers under the guest kernel's cgroup v2 hierarchy, and it detects the delegated controllers when it starts. For limits to be enforced, the controllers must be enabled in `cgroup.subtree_control` **before** `dockerd` starts — enabling them afterwards has no effect on the running daemon without a restart.

On cgroup v2 that write normally fails with `EBUSY`: the kernel's no-internal-process rule forbids enabling controllers on a cgroup that still contains processes, and the shell that starts the daemon lives in that cgroup. The official `docker:dind` image handles this in its `dind` helper by first moving every process into a child cgroup (`/sys/fs/cgroup/init`) and only then enabling the controllers — see the [`dind` wrapper script](https://github.com/moby/moby/blob/master/hack/dind). The image's `dockerd-entrypoint.sh` invokes that helper automatically, so sandboxes started from an official `docker:N-dind` image with the image's entrypoint get the delegation for free. The delegation also needs `/sys/fs/cgroup` to be writable inside the container — one more reason the pod must run privileged (see [Prerequisites](#prerequisites)).

If you start `dockerd` yourself (custom image or platform bootstrap), reproduce the same two steps — process move, then enable controllers — before launching the daemon:

```bash
# --- storage: put /var/lib/docker on a tmpfs sized within the memory limit ---
mkdir -p /var/lib/docker
mount -t tmpfs -o size=4g tmpfs /var/lib/docker

# --- cgroup delegation (must run before dockerd starts) ---
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init
    # Move the processes out of the root group (otherwise writing
    # subtree_control fails with EBUSY), then enable every controller.
    # The loop retries because exec sessions spawn new processes in the
    # root group while the move is in flight.
    while ! {
        xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || :
        sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
            > /sys/fs/cgroup/cgroup.subtree_control
    }; do true; done
fi

# --- start the daemon and wait, bounded ---
dockerd > /tmp/dockerd.log 2>&1 &
DOCKERD_PID=$!

# Bail out if the daemon dies or the wait exceeds 30s — otherwise a wrong
# prerequisite (privileges, mount, storage driver, cgroup) hangs the
# bootstrap forever.
ready=0
for _ in $(seq 1 30); do
    if docker info > /dev/null 2>&1; then ready=1; break; fi
    if ! kill -0 "$DOCKERD_PID" 2>/dev/null; then
        echo "dockerd exited during startup — last log lines:" >&2
        tail -n 20 /tmp/dockerd.log >&2
        exit 1
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "dockerd not ready after 30s — last log lines:" >&2
    tail -n 20 /tmp/dockerd.log >&2
    exit 1
fi
```

Verify the driver actually in use:

```bash
docker info --format '{{.Driver}}'
# Expected: overlay2
```

If this prints `vfs`, the data-directory bootstrap failed and the daemon silently degraded — recreate the sandbox (or fix the mount) rather than trying to migrate in place.

Verify memory enforcement end to end:

```bash
docker run --rm --memory 256m python:3.11 sh -c 'cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes'
# Expected: 268435456
```

If your platform auto-starts the nested daemon for DinD-style images, prefer images whose entrypoint performs the delegation (the official `dind` image does), or add these steps to the platform bootstrap — sandbox users should not need to configure this manually.

## Build-context ownership

`docker build` materializes the build context and preserves file ownership. Contexts assembled from network mounts or Windows-packaged archives can contain UIDs far above 65535; on runtimes that cap the sandbox's UID/GID space with a user namespace, such builds fail with `failed to Lchown ... for UID <N>` (COPY/ADD) or `tar: Cannot change ownership to uid <N>: Invalid argument` (inside RUN). A Kata guest has the complete UID/GID space and is unaffected — but normalizing ownership at packaging time keeps the context portable across runtimes:

```bash
tar --owner=0 --group=0 --numeric-owner -czf ctx.tar.gz dir/   # normalize at pack time
tar --no-same-owner -xzf ctx.tar.gz                            # or drop ownership on extract
rsync --no-owner --no-group src/ dst/                          # or on directory sync
```

Also avoid pointing the build context at the root of a network mount — scope it to the application directory and normalize ownership first.

## Registry pulls under egress control

The nested daemon's pulls are ordinary sandbox egress traffic:

- With a `network_policy` (`dns+nft`) in effect, the registry's DNS name and IPs must be reachable under the policy — registries not covered by the allow rules will fail DNS resolution or connection. Note that allow rules alone do not make DinD work under a policy: the policy wiring strips `NET_ADMIN` from the main container, which the default daemon networking requires — see [DinD and `network_policy` are mutually exclusive](#dind-and-network-policy-are-mutually-exclusive).
- With transparent TLS interception (L7) enabled, HTTPS registry traffic is intercepted. Sandbox bootstrap installs the interception CA into the system trust stores ("trust is delivered, not disabled" — see the [egress architecture doc](/architecture/network/egress)), so a nested daemon using the system CA bundle works as long as it starts **after** bootstrap has delivered the CA. A daemon that bundles its own static CA pool, or one started before CA delivery, will report `x509: certificate signed by unknown authority` on pulls — add the exported CA to that daemon's trust store explicitly.

## Lifecycle and housekeeping

- The nested daemon's images, containers, and volumes live inside the sandbox and follow its lifecycle — they do not survive sandbox deletion or recreation. Keep durable artifacts in mounted volumes or external storage.
- Size the `/var/lib/docker` backing storage for the image caches your workload accumulates, and run `docker system prune` (or configure daemon log rotation) in long-lived sandboxes — a nested image cache is the most common cause of sandbox disk exhaustion.

## Quick troubleshooting reference

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `docker info` shows `Driver: vfs` | `/var/lib/docker` not on a filesystem `overlay2` can use (daemon started against the virtio-fs/devmapper rootfs) | Mount the tmpfs (sized within the memory limit) before starting `dockerd`; recreate the sandbox if it already fell back |
| Daemon dies during startup (readiness wait reports "dockerd exited") | Pod not privileged, or another prerequisite (mount/cgroup) is wrong | Check `/tmp/dockerd.log`; verify the template sets `privileged: true`, the namespace PSA level is `privileged`, and the containerd Kata handler sets `privileged_without_host_devices = true` (see Prerequisites) |
| `--memory`/`mem_limit` fails or is ignored | Controllers not delegated before the daemon started | Run the delegation snippet (process move, then enable controllers) before starting `dockerd`; restart the daemon if it already started without them |
| `failed to Lchown ... for UID <large N>` on COPY | Large-UID owners in the build context under a userns-capped runtime | Normalize ownership (`tar --owner=0 --group=0`) or use a Kata sandbox |
| `x509: certificate signed by unknown authority` on pulls | Interception CA not in the nested daemon's trust store | Start the daemon after CA delivery, or install the exported CA into its trust pool |
| Sandbox disk fills up over time | Unbounded nested image cache | `docker system prune`, size the backing storage, enable log rotation |
