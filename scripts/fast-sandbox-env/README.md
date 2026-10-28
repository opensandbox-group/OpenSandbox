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
./scripts/fast-sandbox-env/integration-env.sh verify-native   # verify matrix on the native template (needs up)
./scripts/fast-sandbox-env/integration-env.sh verify-overlaybd  # overlaybd template build + verify matrix (needs up)
./scripts/fast-sandbox-env/integration-env.sh status          # component / pool / DART / OpenSandbox health
./scripts/fast-sandbox-env/integration-env.sh pool            # re-apply the pool only
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e         # Python SDK e2e suite against the live stack
./scripts/fast-sandbox-env/integration-env.sh idle-e2e        # OSEP-0024 idle-pause/wake suite (needs IDLE_PAUSE=1 up)
./scripts/fast-sandbox-env/integration-env.sh down            # teardown, host left clean
```

### Idle-pause / wake-on-access profile (OSEP-0024)

`IDLE_PAUSE=1 ./scripts/fast-sandbox-env/integration.sh up` additionally deploys an ephemeral Redis (`manifests/idle-pause/redis.yaml`, before the server — the sweeper connects at startup), enables the server `[automation.idle_pause]` sweeper (5s sweep) and the gateway's `--activity-enabled` + `--wake-enabled`, and runs an `idle-pause / wake-on-access` verify stage: an opted-in sandbox (`lifecycle.idlePolicy`, X=30s) is auto-paused with zero traffic, and the next gateway request wakes it (parked while the restore completes). The `idle-e2e` action then runs the SDK suite (`tests/python/tests/test_fsb_idle_pause_e2e.py`) covering the SDK contract only: create with an `IdlePolicy`, auto-pause, and wake-on-access through the SDK's wake retry contract (the multi-round, `wakeOnAccess=false` and Redis-outage behavior lives in the shell verify stage). Sandboxes without `lifecycle.idlePolicy` are never auto-paused or auto-woken regardless of the profile.

### OverlayBD OCI profile (default on)

`OCI_STACK` defaults to 1: one cluster serves both template paths. The env generates a local CA + TLS certificate and deploys an in-cluster `registry:2` (fsb-vm and overlaybd require HTTPS), bakes the CA into the `fsb-fastlet`, `fsb-firecracker-runtime` and sandboxtemplate-builder images, installs the `fast-sandbox` chart's `runtime.oci` node stack, wires the registry credentials and the server's `template_oci_publish_secret`, and renders the pool fastlet with `FAST_SANDBOX_OCI_ENABLED=true` plus the shared tools/socket//dev mounts. The `verify-overlaybd` action builds an overlaybd template (published to the in-cluster registry) and runs the whole verify matrix against it — the same assertions as `verify-native`, but on the overlaybd path (template-mode create resolves `status.manifestRef`, boot is ublk on-demand, and snapshots publish/restore via `oci://` manifests). Nodes need kernel ublk support (`CONFIG_BLK_DEV_UBLK`; `modprobe ublk_drv` when built as a module). `OCI_STACK=0` restores the native-only environment (`verify-overlaybd` becomes a no-op).

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
- **P2P by default**: two kind nodes, each running the runtime-agent with a node-local DART daemon (peer discovery through the headless `dart` Service); the pool spreads one fastlet per node via podAntiAffinity. Each node binds its **own** state-root subdirectory (`/var/lib/fast-sandbox/control-plane`, `/var/lib/fast-sandbox/worker`) at the same container path, so one node's committed image cache never satisfies another node's pull — artifact delivery flows `cache -> peer -> origin` with the origin fetched ~once per 4MiB block cluster-wide on every node's first create. `KIND_SINGLE=1` falls back to one node (cache-only).
- **On-demand loading**: the pool has no `warmImages`; the verify sandbox's create pulls the golden snapshot set through DART. `WARM_IMAGES=1` preheats instead.
- **fast-sandbox @ pinned commit**: env-owned clone at `$WORK/fast-sandbox`, cloned on first `up` and checked out at the pinned commit afterwards; relocate it with `FSB_DIR`. The source itself has no override — bump the commit in `manifests/third-party/fast-sandbox.commit`.

## Layout

```
integration-env.sh          entrypoint: up / verify-native / verify-overlaybd / down / status / pool
architecture.svg             how the environment works (topology, pipeline,
                             P2P delivery, egress actions channel)
manifests/
  cluster/kind-cluster.yaml    two-node kind cluster, KVM/tun/shm mounts,
                               per-node state-root subdirectories
  pool/firecracker-egress-pool.yaml  SandboxPool: egress attached (MITM CA
                               seed mount) + P2P spread
```

Everything else comes from the OpenSandbox Helm charts (`manifests/charts`), rendered with `helm template` from the workdir's values (image tags, artifact-store endpoint, FastPath endpoint, signing key, NodePorts) and applied with plain `kubectl apply` — the cluster keeps no helm state:

| Chart | Provides in this environment |
|---|---|
| `base` | `sandbox.fast.io` + `sandbox.opensandbox.io` CRDs, component RBAC, namespaces |
| `fast-sandbox` | all-in-one control plane (reconcilers + FastPath), janitor, firecracker node installer, runtime-agent + DART |
| `server` | lifecycle server: fsb runtime config (rendered `configToml`), NodePort |
| `ingress-gateway` | fsb provider + FastPath + shared signing key, NodePort |

## Stage order

`up`: preflight → sysctl → fast-sandbox checkout (pinned commit) → build images (6 fast-sandbox images via `manifests/release/build-fast-sandbox.sh` + egress + server + ingress) → XFS StateRoot → kind cluster + node labels (host ports 18080/18081) → RustFS → charts rendered + applied: base + fast-sandbox (CRDs, RBAC, control plane, installer, agent) → credentials → **MITM CA material (Secret + echo leaf cert)** → installer/agent roster asserted → charts rendered + applied: server + ingress gateway → **TLS echo endpoint (kind-network container)** → SandboxTemplate golden image **built through the server `POST /templates` API, CA baked** → SandboxPool (fastlet Ready, egress Ready, pool conditions, actions protocol check, **pinned CA fingerprint**).

`verify-native`: end-to-end verify (templateId create → signed gateway route → execd `/ping` → delete) → pause/resume round-trip (POST pause → poll `Paused` + route released → POST resume → poll `Running` → fresh-route `/ping` → delete) → vault lifecycle → **MITM CA chain verify (pinning three-way, passthrough negative control, SNI credential injection, post-resume injection)** → idle-pause/wake (IDLE_PAUSE profile) → snapshot round trip.

`verify-overlaybd`: overlaybd SandboxTemplate build (oci:// publication) → the same verify matrix against the overlaybd template.

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
| `IMAGE_RUNTIME` | `opensandbox/fsb-firecracker-runtime:dev` | firecracker runtime image (passed to the fast-sandbox chart) |
| — | agent-config defaults pinned by the chart | Firecracker asset version/kernel live in the runtime agent config (`runtime.config` chart value), hot-reloadable |
| `POOL_MIN` / `POOL_MAX` | `2` / `2` | pool capacity (auto `1`/`1` when `KIND_SINGLE=1`) |
| `WARM_IMAGES` | `0` | `1` = preheat pool instead of on-demand first-sandbox pull |
| `SBX_IMAGE` / `EXECD` | `opensandbox/fsb-sandbox-golden:latest` / `opensandbox/execd:latest` | template build inputs |
| `RUSTFS_IMAGE` / `RC_IMAGE` | `rustfs/rustfs:latest` / `rustfs/rc:latest` | RustFS server and CLI images |
| `RUSTFS_PORT` | `19000` | host-side S3 API publish; in-cluster clients use the RustFS container IP on port `9000` |
| `RUSTFS_CONSOLE_PORT` | `19001` | host-side RustFS console publish (human-only; override on port collision) |
| `RUSTFS_AK` / `RUSTFS_SK` | `integration-env` / `integration-env-secret` | local S3 credentials |
| `RUSTFS_ENDPOINT` | auto-discovered kind-network IP on port `9000` | endpoint used by the fast-sandbox and OpenSandbox components |
| `XFS_STATEROOT` / `XFS_SIZE` | `1` / `24G` | reflink StateRoot on/off, virtual size |
| `OSB_PACKAGE` / `OSB_BIN` | `opensandbox-cli` / — | opensandbox CLI for the verify stages: latest from PyPI into the workspace venv, or a preinstalled `osb` binary |
