# fast-sandbox integration environment

One-command OpenSandbox ecosystem integration environment driven from the OpenSandbox repository: a two-node kind cluster (KVM passthrough) running the fast-sandbox Firecracker chain from the source pinned in `manifests/third-party/fast-sandbox.commit`, with the source-built OpenSandbox **egress** sidecar attached to the default pool and the source-built OpenSandbox **server** (fsb runtime) and **ingress gateway** wired on top — verified end to end by the `verify-native` / `verify-overlaybd` actions.

All Kubernetes resources ship from the OpenSandbox Helm charts (`manifests/charts`): `base` (CRDs + RBAC), `fast-sandbox` (control plane, janitor, node installer, runtime-agent + DART), `server` and `ingress-gateway`. The only env-owned manifests left are the kind cluster config and the SandboxPool resource.

The `verify-native` / `verify-overlaybd` actions drive the server through the opensandbox CLI (`osb`, installed fresh from PyPI into the workspace by `up` unless `OSB_BIN` is set): create a sandbox from the golden-image template, reach the in-sandbox execd `/ping` through the signed gateway route, and delete it again. Raw HTTP calls remain only where the CLI has no equivalent (networkpolicy replace, metadata merge-patch, snapshot re-entry fence). The environment is all-or-nothing: running fast-sandbox without the OpenSandbox layers is not a supported shape of this script.

### MITM CA chain (OSEP-0022, always on)

The environment exercises the full MITM trust-anchor chain against the real components — no simulation:

1. **CA material** — a self-signed CA (fresh per run) is provisioned as the versioned Secret `opensandbox-mitm-ca-v1` in the dataplane namespace, per the ops-runbook shape collapsed into one namespace: keypair entries (`tls.crt` + `tls.key`) for the egress seed mount, certificate-only entry (`ca.crt`) for the template bake. The stored certificate is fingerprint-checked against the generated one (content, not name).
2. **Template bake** — the rendered server `config.toml` sets `kubernetes.template_mitm_ca_secret`, so the server fills `spec.egressMitmCertificate` on the template create and the builder installs the certificate into the guest trust store, stamping `platform.egressMitmCA.certSha256` into the published artifact manifest. The template stage asserts the CR field and the lineage fingerprint.
3. **Pool pinning** — the pool's egress container mounts the Secret read-only as its CA seed; the MITM addon starts with the pinned CA (fail-closed, never generates) and exposes its fingerprint on `/healthz`. The pool stage asserts the fingerprint equals the pinned one.
4. **Injection e2e** — a dedicated verify stage creates a sandbox with a policy allowing the CA-signed TLS echo endpoint (a container on the kind network reflecting request headers), first proves the credential-free passthrough (no `X-Api-Key`), then binds a vault credential to the SNI (`X-Api-Key` via the relay) and asserts the guest's own `curl --resolve … https://echo.test` response reflects the injected header — TLS verified against the template-baked CA on every leg, no `-k` anywhere. A pause/resume round trip then repeats the request: trust and vault travel with the snapshot.
5. **Pinning assertion** — template manifest `platform.egressMitmCA` fingerprint == egress `/healthz` fingerprint == the pinned Secret's fingerprint, asserted three-way.

Requires a bare-metal Linux host with KVM (`/dev/kvm`), Docker, Go ≥ 1.25, cgroup v2, and `sudo` for the XFS StateRoot loop mount and sysctl bump. The CLI install requires `uv` (it manages its own ≥ 3.10 interpreter), or set `OSB_BIN` to a preinstalled `osb` binary. Full operational notes (topology, latency characteristics, production caveats): `fast-sandbox/docs/guides/firecracker-integration-env.md`.

## Usage

```bash
./scripts/fast-sandbox-env/integration-env.sh up              # environment: stack + pool + server/ingress + native template
./scripts/fast-sandbox-env/integration-env.sh verify-native   # native boot-path + egress-rules + rootfs-only snapshot verify (needs up)
./scripts/fast-sandbox-env/integration-env.sh verify-overlaybd  # overlaybd template build + full scenario matrix (needs up)
POOL_MODE=resource-aware ./scripts/fast-sandbox-env/integration-env.sh verify-resourceaware  # OSEP-0026 matrix (needs a resource-aware up)
EGRESS_INTERCEPTION_MODE=credential-bound ./scripts/fast-sandbox-env/integration-env.sh verify-credential-bound  # OSEP-0023 delta: switches Pool in-place and runs egress-sensitive stages on existing templates
EGRESS_INTERCEPTION_MODE=credential-bound ./scripts/fast-sandbox-env/integration-env.sh up  # OSEP-0023 credential-bound egress (same variable for the verify actions)
./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound  # OSEP-0023 cost measurements in the current leg (needs up; run after the verify actions)
./scripts/fast-sandbox-env/integration-env.sh status          # component / pool / DART / OpenSandbox health
./scripts/fast-sandbox-env/integration-env.sh pool            # re-apply the pool only
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e         # Python SDK e2e suite against the live stack
./scripts/fast-sandbox-env/integration-env.sh idle-e2e        # OSEP-0024 idle-pause/wake suite (needs IDLE_PAUSE=1 up)
./scripts/fast-sandbox-env/integration-env.sh down            # teardown, host left clean
```

### Credential-bound cost bench (OSEP-0023)

`bench-credential-bound` measures what the `credential-bound` interception mode costs, in whichever leg `EGRESS_INTERCEPTION_MODE` selects (empty/`all` or `credential-bound`). It is implemented in `credential-bound-bench.sh`, which reuses the credential-bound verify's TLS origin fixture, guest client and colocation helpers. Each measurement writes a JSON report under `$WORK/bench/<leg>/` (`BENCH_REPORT_DIR` overrides), assembled by `bench-report.py`: M1 create latency (`create-bench.py`, rounds of creates carrying a network policy), M2 guest TCP+TLS connect latency per path (client `handshake-bench` mode), M3 egress cgroup memory per held bound connection (client `hold-many` mode; `credential-bound` leg only, `NOT_RUN` otherwise), and M4 egress cgroup CPU under passthrough-dominated load. `environment.json` and `bench-summary.json` sit next to them. Reports carry closed-vocabulary fields only. Every guest sample's presented leaf and origin-observed credential are classified, and a path whose evidence contradicts it is `INVALID`. M3 recreates the SandboxPool from a bench-only render (egress memory limit and per-subject budget raised, nothing else changed) and re-applies the default Pool afterwards; the default render and the verify lanes are unchanged. M3 fits only full-target points from the same egress process whose non-reconnecting holders all pass the final release probe; any lost, timed-out or unreadable holder excludes every nonzero K point, listed in `fit_excluded_k_targets`, and leaves the slope unavailable. Its settings record the numeric process-wide TLS capacity and its source from the live mitmdump launcher configuration; unreadable capacity stops M3. Pool readiness compares memory quantities in bytes, so `1024Mi` and `1Gi` are equivalent. M3 needs the TLS origin container, which inherits the Docker daemon's default open-file limit, to have a soft limit well above the largest `BENCH_MEM_STEPS` value; with a 1024 soft limit, K = 2000 ends `PARTIAL`. The action fails when a measurement is `FAILED`, `INVALID` or `MISSING`; `PARTIAL` (for example, M3 could not reach every step) is recorded with the largest K reached and the closed failure kinds. In CI the integration job runs it after the verify steps when the `bench` dispatch input is true or the PR carries the `fast-sandbox-bench` label (add it before `ok-to-test` triggers the run), and uploads `fast-sandbox-bench-<leg>-<attempt>`.

### Idle-pause / wake-on-access profile (OSEP-0024)

`IDLE_PAUSE=1 ./scripts/fast-sandbox-env/integration-env.sh up` additionally deploys an ephemeral Redis (`manifests/idle-pause/redis.yaml`, before the server — the sweeper connects at startup), enables the server `[automation.idle_pause]` sweeper (5s sweep) and the gateway's `--activity-enabled` + `--wake-enabled`; the `idle-pause / wake-on-access` verify stage then runs in the overlaybd scenario matrix: an opted-in sandbox (`lifecycle.idlePolicy`, X=30s) is auto-paused with zero traffic, and the next gateway request wakes it (parked while the restore completes). The `idle-e2e` action then runs the SDK suite (`tests/python/tests/test_fsb_idle_pause_e2e.py`) covering the SDK contract only: create with an `IdlePolicy`, auto-pause, and wake-on-access through the SDK's wake retry contract (the multi-round, `wakeOnAccess=false` and Redis-outage behavior lives in the shell verify stage). Sandboxes without `lifecycle.idlePolicy` are never auto-paused or auto-woken regardless of the profile.

### OverlayBD OCI profile (default on)

The node ublk warm pool is enabled at low/high watermarks of 4/8 with 2 GiB devices, matching this environment's explicit `--resource disk=2Gi` template build setting. `verify-overlaybd` waits for idle devices, logs `/v1/pool` and daemon restart counts before and after the six template creates/deletes, and requires at least six new hits with no new misses, daemon restarts or Pod replacement. The Helm chart also defaults to 2 GiB devices because the OpenSandbox template API uses `resourceLimits.disk=2Gi` when omitted. When changing the template build size, change `runtime.oci.poolSizeGB` to the built rootfs capacity as well: exact capacity matching is required, even when the pool device is larger. `runtime.oci.poolLow=0` disables pooling.

`OCI_STACK` defaults to 1: one cluster serves both template paths. The env generates a local CA + TLS certificate and deploys an in-cluster `registry:2` (fsb-vm and overlaybd require HTTPS), bakes the CA into the `fsb-fastlet`, `fsb-firecracker-runtime` and sandboxtemplate-builder images, installs the `fast-sandbox` chart's `runtime.oci` node stack, wires the registry credentials and the server's `template_oci_publish_secret`, and renders the pool fastlet with `FAST_SANDBOX_OCI_ENABLED=true` plus the shared tools/socket//dev mounts. The `verify-overlaybd` action builds an overlaybd template (published to the in-cluster registry) and runs the full scenario matrix against it — the storage-independent stages run once, here, while `verify-native` keeps the boot-path, egress-rules and native-lane rootfs-only snapshot stages (template-mode create resolves `status.manifestRef`, boot is ublk on-demand, and snapshots publish/restore via `oci://` manifests). Nodes need kernel ublk support (`CONFIG_BLK_DEV_UBLK`; `modprobe ublk_drv` when built as a module). `OCI_STACK=0` restores the native-only environment (`verify-overlaybd` becomes a no-op).

After `up`, point any OpenSandbox SDK at `http://127.0.0.1:18080` with the `OPEN-SANDBOX-API-KEY: fast-sandbox-env` header; sandbox endpoints are signed `f1.*` header routes served by the gateway at `http://127.0.0.1:18081`. The installed CLI works against the same stack, e.g. `OPEN_SANDBOX_DOMAIN=127.0.0.1:18080 OPEN_SANDBOX_API_KEY=fast-sandbox-env osb sandbox list`.

The `verify-native` / `verify-overlaybd` stages are also available as Python SDK e2e tests (template create → gateway ping → networkpolicy convergence, lifecycle ops, pause/resume, snapshot round trip):

```bash
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e   # needs uv on PATH
```

or, manually: `cd tests/python && OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$(cat "$WORK/overlaybd-template-id")" make test-fsb` (requires `OPENSANDBOX_TEST_FSB_TEMPLATE_ID`; `sdk-e2e` and `idle-e2e` run against the overlaybd template stored in `$WORK/overlaybd-template-id` when `OCI_STACK=1`, falling back to the native template in `$WORK/template-id` under `OCI_STACK=0`).

## Wiring

```
SDK ──HTTP──> server (source-built, runtime.type=fsb)
              │ POST /sandboxes ──gRPC──> FastPath ──> fastlet ──> firecracker sandbox
              │                                                    (golden image + execd, egress attached)
              └─ GET endpoints/{port}: signed f1.* route (HMAC, key shared with ingress)
SDK ──header──> ingress gateway (source-built, --provider-type=fast-sandbox)
              │ ResolveEndpoint ──gRPC──> FastPath
              └─> fastlet-proxy ──> sandbox execd :44772
```

- **server / ingress / egress** are built from this repository (`server/Dockerfile`, `components/ingress`, `components/egress`) and kind-loaded; the server and the gateway are installed through `manifests/charts/server` and `manifests/charts/ingress-gateway`.
- **execd** is baked into the SandboxTemplate golden image. Templates are created through the server's `POST /templates` API (server config injects `EXECD` as the build's execd image, kernel stays the builder default); `SBX_IMAGE` / `EXECD` select the build inputs.

## Defaults

- **Pool with egress**: `firecracker-egress-pool` (firecracker runtime, `poolMin=2`) carries the OpenSandbox egress container in every fastlet Pod netns — fast-sandbox profile, `dns+nft` mode, MITM addon pinned to the CA seed mount — wired through the Sandbox Actions channel (`infraComponents` host-process entry + `actionHandlers` egress@18080 with the runtime-ready / data-plane-ready hooks).
- **ResourceAware pool fixture (OSEP-0026)**: `POOL_MODE=resource-aware` swaps the default pool for `firecracker-egress-resource-aware-pool` (`manifests/pool/firecracker-egress-resource-aware-pool.yaml`) — identical except `sandboxResources.mode: ResourceAware` with `overcommitRatio` cpu `2` / memory `2`, so FastPath derives each sandbox's committed request (`ceil(limit / ratio)`, a default-shape sandbox commits 500m CPU / 256Mi into a 4 CPU / 2Gi per-fastlet budget) instead of fixed slot accounting. `POOL_NAME` and the server's `fastpath_resource_pool` follow the profile.
- **P2P by default**: two kind nodes, each running the runtime-agent with a node-local DART daemon (peer discovery through the headless `dart` Service); the pool spreads one fastlet per node via podAntiAffinity. Each node binds its **own** state-root subdirectory (`/var/lib/fast-sandbox/control-plane`, `/var/lib/fast-sandbox/worker`) at the same container path, so one node's committed image cache never satisfies another node's pull — artifact delivery flows `cache -> peer -> origin` with the origin fetched ~once per 4MiB block cluster-wide on every node's first create. `KIND_SINGLE=1` falls back to one node (cache-only).
- **On-demand loading**: the pool has no `warmImages`; the verify sandbox's create pulls the golden snapshot set through DART. `WARM_IMAGES=1` preheats instead.
- **fast-sandbox @ pinned commit**: env-owned clone at `$WORK/fast-sandbox`, cloned on first `up` and checked out at the pinned commit afterwards; relocate it with `FSB_DIR`. The source itself has no override — bump the commit in `manifests/third-party/fast-sandbox.commit`.

### Native-template disk hotplug probe

The **Fast Sandbox Integration Tests** workflow has an opt-in `verify_disk_hotplug` input for [#2941](https://github.com/opensandbox-group/OpenSandbox/issues/2941). Dispatch a ref containing the probe and set `fsb_commit` to the full SHA of the fast-sandbox candidate. On a relevant OpenSandbox PR, maintainers can select the same probe by applying both `ok-to-test` and `test-disk-hotplug`. The default matrix leg tests the fast-sandbox pin from that PR. The standalone command after `integration-env.sh up` is `sudo -E ./scripts/fast-sandbox-env/verify/disk-hotplug.sh`. The runner needs `mkfs.ext4` and `debugfs` from e2fsprogs.

At fast-sandbox commit `96e41031`, the [template builder](https://github.com/opensandbox-group/fast-sandbox/blob/96e41031cb091d099ab6f15d4da5528835d065b0/cmd/sandboxtemplate-builder/snapshot_stage.go#L133) boots with `pci=off` and the [restore launcher](https://github.com/opensandbox-group/fast-sandbox/blob/96e41031cb091d099ab6f15d4da5528835d065b0/internal/runtime/firecracker/launcher.go#L189-L209) omits `--enable-pci`. This pin is expected to fail the guest PCI check. A candidate needs a freshly built PCI-capable template, guest virtio-pci and virtio-blk drivers, and a compatible restore path.

The probe starts a named exec session, observes two ticks from one live command, then hotplugs a disposable ext4 image and rescans PCI in the guest. The original command must see the seeded marker at `/data`, write back to the disk, and finish with the same PID and an uninterrupted sequence. After unmount, the runner checks the write directly in the image. CSI provisioning and PVC lifetime are separate tests.

## Layout

```
integration-env.sh          entrypoint: up / verify-native / verify-overlaybd / down / status / pool
credential-bound-bench.sh   OSEP-0023 cost measurements (bench-credential-bound action)
bench-report.py              report assembly for the bench (percentiles, fits, evidence classes)
architecture.svg             how the environment works (topology, pipeline,
                             P2P delivery, egress actions channel)
manifests/
  cluster/kind-cluster.yaml    two-node kind cluster, KVM/tun/shm mounts,
                               per-node state-root subdirectories
  pool/firecracker-egress-pool.yaml  SandboxPool: egress attached (MITM CA
                               seed mount) + P2P spread
  pool/firecracker-egress-resource-aware-pool.yaml  OSEP-0026 ResourceAware
                               fixture: same pool + mode/overcommitRatio
                               (POOL_MODE=resource-aware)
```

Everything else comes from the OpenSandbox Helm charts (`manifests/charts`), rendered with `helm template` from the workdir's values (image tags, artifact-store endpoint, FastPath endpoint, signing key, NodePorts) and applied with plain `kubectl apply` — the cluster keeps no helm state:

| Chart | Provides in this environment |
|---|---|
| `base` | `sandbox.fast.io` + `sandbox.opensandbox.io` CRDs, component RBAC, namespaces |
| `fast-sandbox` | all-in-one control plane (reconcilers + FastPath), janitor, firecracker node installer, runtime-agent + DART |
| `server` | lifecycle server: fsb runtime config (rendered `configToml`), NodePort |
| `ingress-gateway` | fsb provider + FastPath + shared signing key, NodePort |
| `node-agent` | source-built vm-console collector (DaemonSet on both runtime nodes, file sink; `sources=[vm-console]`, `vmConsoleRoot` = the fast-sandbox chart's `runtime.stateRoot`) |

## Stage order

`up`: preflight → sysctl → fast-sandbox checkout (pinned commit) → build images (6 fast-sandbox images via `manifests/release/build-fast-sandbox.sh` + egress + server + ingress + **nodeagent from source**) → XFS StateRoot → kind cluster + node labels (host ports 18080/18081) → RustFS → charts rendered + applied: base + fast-sandbox (CRDs, RBAC, control plane, installer, agent) → credentials → **MITM CA material (Secret + echo leaf cert)** → installer/agent roster asserted → charts rendered + applied: server + ingress gateway → **node-agent DaemonSet (vm-console collection)** → **TLS echo endpoint (kind-network container)** → SandboxTemplate golden image **built through the server `POST /templates` API, CA baked** → SandboxPool (fastlet Ready, egress Ready, pool conditions, actions protocol check, **pinned CA fingerprint**).

`verify-native`: end-to-end verify (templateId create → signed gateway route → execd `/ping` → delete; three create/delete cycles: cold DART pull, cross-node peer pull, one steady-state warm) → egress network-security-rules verify (configmap → hot load → kernel → live traffic, then emptied again so nothing leaks into later stages) → rootfs-only snapshot verify (native lane, default-on; `ROOTFS_ONLY=0` skips, `OCI_STACK=0` skips: source sandbox → `mode: rootfsOnly` snapshot → server row and SandboxSnapshot CR both report RootfsOnly with an `s3://` manifest → source survives the capture → cold-boot restore → pause/resume full-checkpoint round on the restored descendant) → **vm-console collection + finalize verify** (a marker echoed to the guest's `/dev/console` must land in the node-agent's file sink under `_streams/vm-console/`; the matrix's last stage asserts the killed e2e sandbox's stream finalized with `status: complete` — the Source's 30s disappearance grace elapses behind the network-security-rules stage in between).

`verify-overlaybd`: overlaybd SandboxTemplate build (oci:// publication) → the full scenario matrix against the overlaybd template: end-to-end verify (six create/delete cycles proving ublk warm-device reuse) → **execd-as-init in-guest process verify** (PID 1 = guest init; execd `--init` supervises the workload; orphan reparented and reaped; hardening report `init_mode=subreaper`) → **hardened golden image + SandboxTemplate build (OSEP-0018: the hardened source image — launcher prelude + `EXECD_ISOLATION_CONFIG`-targeted TOML baked at `/opt/opensandbox/` — is built from this checkout, pushed to the in-cluster registry, and feeds the hardened template; the plain template stays on the operator-published `SBX_IMAGE`) → execd hardening floor verify** (launcher present; TOML loaded — `cap_drop`/`seccomp` active; the launcher-wrapped `/command` workload runs with `CapEff=0`/`CapBnd=0`/`Seccomp=2`/`NoNewPrivs=1` as root and execd's config env stripped; Landlock `/tmp`-writable + `/etc/passwd`-read-only + exec probes when active, honest `unsupported` otherwise; capabilities report `init_mode=subreaper`/`signal_shield=false`; sustained fork-heavy mix keeps a bounded, zombie-free process table under the subreaper; the custom-policy, `EXECD_INIT`-drift and missing-`CAP_SETPCAP` pins stay on the docker/k8s legs) → pause/resume round-trip (POST pause → poll `Paused` + route released → POST resume → poll `Running` → fresh-route `/ping` → delete) → vault lifecycle → **MITM CA chain verify (pinning three-way, passthrough negative control, SNI credential injection, post-resume injection)** → exchange resolver (OSEP-0025: relay → per-request resolve → guest) → idle-pause/wake (IDLE_PAUSE profile) → snapshot round trip → rootfs-only snapshot verify (OCI manifest pair, default-on; `ROOTFS_ONLY=0` skips) → **vm-console finalize verify**.

`verify-resourceaware` (OSEP-0026, needs `POOL_MODE=resource-aware up`): distinctly-shaped template (2 vCPU / 1Gi) create → Sandbox CR `spec.resources.limits` + cgroup leaf `cpu.max`/`cpu.weight`/`pids.max` + aggregate `memory.max` asserted separately → oversized template (8 vCPU) → permanent 400 → template → snapshot → restore round-trip replaying the recorded limits through the composite path → capacity saturation (9 concurrent shaped sandboxes vs 2×4 slots) → retryable 429.

Every stage logs to `$WORK/logs/`; failures dump component logs to `logs/failure-<task>-<ts>.txt`.

## Environment variables (selection)

The RustFS migration replaces the previous `MINIO_*` and `MC_IMAGE` overrides. Before upgrading an existing environment, run `down` with the previous script.

| Variable | Default | Meaning |
|---|---|---|
| `WORK` | `/data/fast-sandbox-env` when `/data` exists, else `$PWD/.fast-sandbox-env` | workspace + logs + XFS loop + RustFS data (heavy: prefer a big volume) |
| `FSB_DIR` | `$WORK/fast-sandbox` | fast-sandbox checkout (env-owned clone, created when missing) |
| `KIND_CLUSTER` | `fast-sandbox-integration` | kind cluster name |
| `KIND_SINGLE` | `0` | `1` = single node (cache-only, no peer traffic) |
| `DOCKER_MIRROR` | — | comma list injected as docker.io containerd mirrors |
| `EGRESS_IMAGE` | `docker.io/opensandbox/egress:latest` | egress image tag (built from `components/egress`) |
| `SERVER_HOST_PORT` / `GATEWAY_HOST_PORT` | `18080` / `18081` | host-side publishes for server / gateway (loopback only) |
| `SERVER_IMAGE` | `docker.io/opensandbox/server:env` | server image tag (built from `server/`) |
| `INGRESS_IMAGE` | `docker.io/opensandbox/ingress:env` | ingress image tag (built from `components/ingress`) |
| `NODEAGENT_IMAGE` | `opensandbox/nodeagent:env` | node-agent image tag (built from `components/nodeagent`) |
| `NODEAGENT_CLUSTER_ID` | `fsb-integration` | cluster name stamped into collected records; the vm-console verify derives the file-sink tree from it |
| `IMAGE_RUNTIME` | `opensandbox/fsb-firecracker-runtime:dev` | firecracker runtime image (passed to the fast-sandbox chart) |
| — | agent-config defaults pinned by the chart | Firecracker asset version/kernel live in the runtime agent config (`runtime.config` chart value), hot-reloadable |
| `POOL_MIN` / `POOL_MAX` | `2` / `2` | pool capacity (auto `1`/`1` when `KIND_SINGLE=1`) |
| `POOL_MODE` | `legacy` | `resource-aware` = OSEP-0026 ResourceAware pool fixture (`firecracker-egress-resource-aware-pool`: `sandboxResources.mode: ResourceAware` + cpu `2` / memory `2` `overcommitRatio`); `POOL_NAME` follows the profile |
| `EGRESS_INTERCEPTION_MODE` | — | OSEP-0023 (experimental): `all` or `credential-bound` renders `OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE` into the pool's egress container (empty = Pool rendered unchanged; other values fail before any action). `credential-bound` adds `verify/credential-bound.sh` to `verify-native` and `verify-overlaybd` (standalone: `verify/credential-bound.sh`; `CB_COLOCATE_ATTEMPTS` default `6` bounds the creates that place the unbound sandbox on the bound one's Fastlet Pod, `CB_REUSE_ATTEMPTS` default `4` bounds the address-reuse attempts). The overlaybd lane runs the exchange stage in both interception modes; `NOT_RUN` items are listed after the stage timings and never count as passed |
| `BENCH_CREATE_N` / `BENCH_CREATE_CONCURRENCY` / `BENCH_CREATE_ROUNDS` | `200` / `10` / `3` | `bench-credential-bound` M1: creates per round, concurrency, rounds |
| `BENCH_HANDSHAKES` / `BENCH_HANDSHAKE_WARMUP` | `200` / `10` | M2: timed and discarded handshakes per path (at most 5000) |
| `BENCH_MEM_STEPS` / `BENCH_MEM_SANDBOXES` / `BENCH_MEM_SETTLE_SECONDS` | `500 1000 2000` / `4` / `20` | M3: increasing cumulative connection targets, colocated holder sandboxes, settle time before each cgroup read |
| `BENCH_EGRESS_MEMORY_LIMIT` / `BENCH_FSB_TLS_CAPACITY` | `2Gi` / `1024` | M3 bench-only Pool render: egress memory limit and `OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY` (must fit the largest step's per-sandbox share) |
| `BENCH_CPU_UNBOUND` / `BENCH_CPU_BOUND` / `BENCH_CPU_IDLE_SECONDS` | `1900` / `100` / `30` | M4: short HTTPS requests from the unbound and bound sandbox, idle window for the correction |
| `WARM_IMAGES` | `0` | `1` = preheat pool instead of on-demand first-sandbox pull |
| `SBX_IMAGE` / `EXECD` | `opensandbox/fsb-sandbox-golden:latest` / `opensandbox/execd:latest` | template build inputs; both are published images the template builder pulls from a registry (operator-maintained). `SBX_IMAGE` feeds the plain golden template; the hardened template no longer depends on it (see `HARDENED_SBX_*` below — the lane builds that source image from this checkout) |
| `HARDENED_SBX_PUSH` / `HARDENED_SBX_REF` / `HARDENED_SBX_PUSH_PORT` | `1` / `<OCI registry>/sandbox/templates/sandbox-golden-hardened:dev` / `18443` | the hardening lane builds the hardened golden image from this checkout's `Dockerfile.sandbox-test-image` (launcher prelude + hardened isolation TOML baked at `/opt/opensandbox/`) and pushes it to the in-cluster OCI registry over a port-forward (runner borrows the registry DNS name in `/etc/hosts` + trusts the env CA via `/etc/docker/certs.d`; both restored after the build). `HARDENED_SBX_PUSH=0` escapes to `SBX_IMAGE` for an operator whose published image already carries the bake |
| `EXECD_RUN_AS_INIT` | `true` | OSEP-0018 execd-as-init: templates bake `EXECD_INIT=1` (pinned by the init-mode verify stage); `false` = classic topology, that stage fails. |
| `RUSTFS_IMAGE` / `RC_IMAGE` | `rustfs/rustfs:latest` / `rustfs/rc:latest` | RustFS server and CLI images |
| `RUSTFS_PORT` | `19000` | host-side S3 API publish; in-cluster clients use the RustFS container IP on port `9000` |
| `RUSTFS_CONSOLE_PORT` | `19001` | host-side RustFS console publish (human-only; override on port collision) |
| `RUSTFS_AK` / `RUSTFS_SK` | `integration-env` / `integration-env-secret` | local S3 credentials |
| `RUSTFS_ENDPOINT` | auto-discovered kind-network IP on port `9000` | endpoint used by the fast-sandbox and OpenSandbox components |
| `XFS_STATEROOT` / `XFS_SIZE` | `1` / `24G` | reflink StateRoot on/off, virtual size |
| `OSB_PACKAGE` / `OSB_BIN` | `opensandbox-cli` / — | opensandbox CLI for the verify stages: latest from PyPI into the workspace venv, or a preinstalled `osb` binary |
