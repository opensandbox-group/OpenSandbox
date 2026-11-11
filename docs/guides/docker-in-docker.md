---
title: Docker-in-Docker on Kata Sandboxes
description: Run a nested Docker daemon inside a Kata-isolated OpenSandbox sandbox — prerequisites, storage driver selection, cgroup delegation, and common pitfalls.
---

# Running Docker Inside Kata Sandboxes (Docker-in-Docker)

### Running Docker Inside Kata Sandboxes (Docker-in-Docker)

Many workloads need their own Docker daemon inside the sandbox: building images, running `docker compose` stacks, or CI jobs that spawn helper containers. This section covers how to run a nested daemon (Docker-in-Docker, DinD) inside a sandbox that is itself isolated by Kata Containers, and the three pitfalls that account for most failures: storage-driver selection, cgroup delegation, and build-context ownership.

### Why Kata works well for nested Docker

Under Kata the sandbox runs as a full VM, so a nested `dockerd` runs against the guest kernel — no nested virtualization is involved. Compared with user-namespace-based container runtimes, the guest kernel gives the nested daemon:

- **A complete kernel feature set**: full `iptables`/`nft` tables (including `nat`), so nested containers network normally.
- **An unrestricted UID/GID space**: the guest `uid_map` is complete, so build contexts and images with large numeric owners (common with network-mounted sources or Windows-packaged archives) are handled natively.
- **A real cgroup hierarchy**: `docker run --memory` / `--cpus` and `mem_limit`/`cpus` in `docker compose` are enforced by the guest kernel's cgroup v2 — provided delegation is configured (below).

### Prerequisites

- The sandbox image must contain the daemon and CLI (for example `docker:27-dind`, or a custom image with `dockerd` and `docker` installed).
- The server must be configured with a Kata runtime (`secure_runtime.type = "kata"` with `k8s_runtime_class = "kata-qemu"` or `"kata-fc"`), as described in the Kubernetes Mode section above.
- **Capabilities**: rootful `dockerd` needs `NET_ADMIN` for its bridge and NAT/iptables setup, and the mount and cgroup writes below need `SYS_ADMIN` — the main sandbox container gets neither by default, and how (and whether) they can be granted depends on the workload provider path:

  - **Kubernetes mode (BatchSandbox provider)**: the server-injected main sandbox container carries no `securityContext` of its own, so grant the capabilities in the **BatchSandbox CR template file** configured at `[kubernetes].batchsandbox_template_file` — not a `SandboxTemplate` CRD (that CRD has no main-container pod spec). This is a partial BatchSandbox manifest, like the [shipped AKS example](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/aks-kata/batchsandbox-template-configmap.yaml); the provider merges the template's `sandbox` container `securityContext` (or the first container's) into the runtime container, and template values win wherever the runtime does not set one:

    ```yaml
    # BatchSandbox CR template file ([kubernetes].batchsandbox_template_file)
    spec:
      template:
        spec:
          containers:
            - name: sandbox   # the provider merges this container's securityContext into the runtime main container
              securityContext:
                capabilities:
                  add: ["NET_ADMIN", "SYS_ADMIN"]
    ```

    The target namespace must also run Pod Security Admission at the `privileged` enforce level (`pod-security.kubernetes.io/enforce=privileged`); `baseline` and `restricted` reject added capabilities. Without the grant, a `docker:27-dind` sandbox starts, but the daemon cannot initialize its bridge/iptables/cgroup setup and dies during the readiness wait — the [troubleshooting table](#quick-troubleshooting-reference) row "daemon dies during startup" is what this looks like.

    ::: warning The template grant is provider-wide — it re-arms `NET_ADMIN` inside `network_policy` sandboxes
    Every sandbox pod created by a server using this template gets `NET_ADMIN`, not just DinD sandboxes. When a request carries a `network_policy`, the policy wiring adds `drop: [NET_ADMIN]` to the same container — but the provider's security-context merge keeps the template's `add` alongside the policy's `drop` (`_merge_security_context` in `services/k8s/batchsandbox_provider.py`), and Kubernetes applies additions after drops, so `NET_ADMIN` is restored and the workload can rewrite the shared pod-netns rules, bypassing the egress sidecar. The server does not currently detect or reject this combination: never put `NET_ADMIN` in a template that also serves `network_policy` requests — serve DinD sandboxes from a separate template (or a separate server deployment) than policy-governed sandboxes, and verify after creation that a policy sandbox's emitted pod does not carry `NET_ADMIN` in both `capabilities.add` and `capabilities.drop`. See [Limitations](#limitations).
    :::

  - **Docker mode (Docker provider)**: rootful DinD is not supported on this path. The server drops `NET_ADMIN` and `SYS_ADMIN` from sandbox containers by default (`[docker].drop_capabilities`) and exposes no capability-add option for them — removing the entries only restores the container engine's default capability set, which does not include `NET_ADMIN` either. Run nested-daemon workloads on the Kubernetes (BatchSandbox) path above.

### Limitations

- **`network_policy` and DinD are incompatible with the default daemon networking.** When a sandbox request carries a `network_policy`, the server deliberately strips `NET_ADMIN` from the main sandbox container (`build_security_context_for_sandbox_container` in `services/k8s/egress_helper.py`) so the egress sidecar stays the only writer of the pod's network rules. Rootful `dockerd` needs `NET_ADMIN` for its bridge and NAT setup, so under the default configuration the nested daemon cannot bring up its network — this combination is unsupported. Granting `NET_ADMIN` back to the workload is not an option: the capability would let the sandbox mutate the same shared pod-netns rules the policy depends on, defeating the isolation. This also holds for **template-level grants**: a `NET_ADMIN` entry in the BatchSandbox template file's `capabilities.add` applies to every pod from that template, and on a policy sandbox the provider's security-context merge keeps it alongside the policy's `drop` while Kubernetes applies additions after drops — so `NET_ADMIN` comes back and the policy is silently defeated. Keep `NET_ADMIN` out of any template that serves `network_policy` requests (see the Prerequisites warning above). Allow-listing the registry endpoints (below) is necessary but not sufficient — it fixes name resolution and reachability, not the daemon's own bridge setup. A `dockerd --bridge=none --iptables=false` daemon starts without `NET_ADMIN`, but nested containers then share the sandbox's network namespace and get no isolated networks or port publishing — treat that as an untested escape hatch, not a supported configuration.

### Storage driver: put `/var/lib/docker` on a real filesystem

`overlay2` cannot operate on top of overlayfs — and the sandbox container's own root filesystem is typically overlayfs (Docker/containerd overlay snapshots). If `dockerd` starts with its data directory on the container rootfs, it falls back to the `vfs` driver: functional, but image pulls and builds are far slower and far more disk-hungry.

Bootstrap the data directory onto a filesystem that supports `overlay2` before starting the daemon:

```bash
mkdir -p /var/lib/docker
mount -t ext4 /dev/<data-device> /var/lib/docker   # dedicated data volume
# or, for short-lived sandboxes without a data volume:
# mount -t tmpfs -o size=32g tmpfs /var/lib/docker

dockerd > /tmp/dockerd.log 2>&1 &
DOCKERD_PID=$!

# Wait for readiness with a bounded loop: bail out if the daemon dies or
# the wait exceeds 30s, otherwise a wrong prerequisite (mount, storage
# driver, cgroup, capability) hangs the bootstrap forever.
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

If this prints `vfs`, the data-directory bootstrap failed and the daemon silently degraded — recreate the sandbox (or fix the mount) rather than trying to migrate in place. In our testing, `overlay2` on a dedicated ext4 data volume is roughly **2.7–2.9× faster** than `vfs` for image pull/build operations.

### cgroup delegation: make `--memory` and `--cpus` enforce

The nested daemon creates cgroups for its containers under the guest kernel's cgroup v2 hierarchy. For limits to work, the delegated subtree must have the relevant controllers enabled *before* `dockerd` starts:

```bash
# Inside the sandbox, before starting dockerd:
cat /sys/fs/cgroup/cgroup.controllers
# Enable controllers for the subtree the daemon will use:
echo "+memory +cpuset +cpu +pids" > /sys/fs/cgroup/cgroup.subtree_control
```

If the controllers are missing from `cgroup.subtree_control`, nested `--memory` limits either fail at container start or — worse on some kernel versions — are accepted but not enforced.

Verify enforcement end to end:

```bash
docker run --rm --memory 256m python:3.11 sh -c 'cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes'
# Expected: 268435456
```

If your platform auto-starts the nested daemon for DinD-style images, this delegation belongs in that bootstrap step — sandbox users should not need to configure it manually.

### Build-context ownership pitfalls

`docker build` materializes the build context and preserves file ownership. Contexts assembled from network mounts or Windows-packaged archives can contain UIDs far above 65535:

- Under **user-namespace-capped runtimes** (e.g., sysbox), the sandbox UID/GID space is limited to 0–65535, and such builds fail with `failed to Lchown ... for UID <N>` (COPY/ADD) or `tar: Cannot change ownership to uid <N>: Invalid argument` (inside RUN). The `try increasing /etc/subuid` hint in the first error is misleading — the ceiling comes from the user namespace, not from subuid configuration.
- Under **Kata** this ceiling does not exist; builds with large-UID owners work as-is.

Runtime-independent fixes (recommended at packaging time):

```bash
tar --owner=0 --group=0 --numeric-owner -czf ctx.tar.gz dir/   # normalize at pack time
tar --no-same-owner -xzf ctx.tar.gz                            # or drop ownership on extract
rsync --no-owner --no-group src/ dst/                          # or on directory sync
```

Also avoid pointing the build context at the root of a network mount — scope it to the application directory and normalize ownership first.

### Registry pulls under egress control

The nested daemon's pulls are ordinary sandbox egress traffic:

- With a `network_policy` (`dns+nft`) in effect, the registry's DNS name and IPs must be reachable under the policy — registries not covered by the allow rules will fail DNS resolution or connection. Note that allow rules alone do not make DinD work under a policy: the policy wiring strips `NET_ADMIN` from the main container, which the default daemon networking requires — see [Limitations](#limitations) above.
- With transparent TLS interception (L7) enabled, HTTPS registry traffic is intercepted. Sandbox bootstrap installs the interception CA into the system trust stores ("trust is delivered, not disabled" — see the [egress architecture doc](/architecture/network/egress)), so a nested daemon using the system CA bundle works as long as it starts **after** bootstrap has delivered the CA. A daemon that bundles its own static CA pool, or one started before CA delivery, will report `x509: certificate signed by unknown authority` on pulls — add the exported CA to that daemon's trust store explicitly.

### Lifecycle and housekeeping

- The nested daemon's images, containers, and volumes live inside the sandbox and follow its lifecycle — they do not survive sandbox deletion or recreation. Keep durable artifacts in mounted volumes or external storage.
- Size the `/var/lib/docker` backing volume for the image caches your workload accumulates, and run `docker system prune` (or configure daemon log rotation) in long-lived sandboxes — a nested image cache is the most common cause of sandbox disk exhaustion.

### Quick troubleshooting reference

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `docker info` shows `Driver: vfs` | `/var/lib/docker` not on a filesystem supporting `overlay2` | Mount the data directory before starting `dockerd`; recreate the sandbox if it already fell back |
| Daemon dies during startup (readiness wait reports "dockerd exited") | Missing capability (`NET_ADMIN`/`SYS_ADMIN`) on the Kubernetes path, or a mount/cgroup prerequisite is wrong | Check `/tmp/dockerd.log`; on Kubernetes verify the BatchSandbox template file (`[kubernetes].batchsandbox_template_file`) carries the capabilities and the namespace PSA level is `privileged` (see Prerequisites) |
| `--memory`/`mem_limit` fails or is ignored | cgroup controllers not enabled in the delegated subtree | `echo "+memory +pids" > /sys/fs/cgroup/cgroup.subtree_control` before starting `dockerd` |
| `failed to Lchown ... for UID <large N>` on COPY | Large-UID owners in the build context under a userns-capped runtime | Normalize ownership (`tar --owner=0 --group=0`) or use a Kata sandbox |
| `x509: certificate signed by unknown authority` on pulls | Interception CA not in the nested daemon's trust store | Start the daemon after CA delivery, or install the exported CA into its trust pool |
| Sandbox disk fills up over time | Unbounded nested image cache | `docker system prune`, size the data volume, enable log rotation |
