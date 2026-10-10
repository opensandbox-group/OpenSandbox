# fast-sandbox integration environment

One-command OpenSandbox ecosystem integration environment driven from the
OpenSandbox repository: a two-node kind cluster (KVM passthrough) running
the fast-sandbox Firecracker chain from the source pinned in
`manifests/third-party/fast-sandbox.commit`, with the source-built
OpenSandbox **egress** sidecar attached to the default pool and the
source-built OpenSandbox **server** (fsb runtime) and **ingress gateway**
wired on top — verified end to end on every `up`.

All Kubernetes resources ship from the OpenSandbox Helm charts
(`manifests/charts`): `base` (CRDs + RBAC), `fast-sandbox` (control plane,
janitor, node installer, runtime-agent + DART), `server` and
`ingress-gateway`. The only env-owned manifests left are the kind cluster
config and the SandboxPool resource.

The last `up` stages drive the server through the opensandbox CLI (`osb`,
installed fresh from PyPI into the workspace unless `OSB_BIN` is set):
create a sandbox from the golden-image template, reach the in-sandbox
execd `/ping` through the signed gateway route, and delete it again. Raw
HTTP calls remain only where the CLI has no equivalent (networkpolicy
replace, metadata merge-patch, snapshot re-entry fence). The environment
is all-or-nothing: running fast-sandbox without the OpenSandbox layers is
not a supported shape of this script.

Requires a bare-metal Linux host with KVM (`/dev/kvm`), Docker, Go ≥ 1.25,
cgroup v2, and `sudo` for the XFS StateRoot loop mount and sysctl bump.
The CLI install requires `uv` (it manages its own ≥ 3.10 interpreter), or
set `OSB_BIN` to a preinstalled `osb` binary.
Full operational notes (topology, latency characteristics, production
caveats): `fast-sandbox/docs/guides/firecracker-integration-env.md`.

## Usage

```bash
./scripts/fast-sandbox-env/integration-env.sh up        # full stack + end-to-end + pause/resume verify
./scripts/fast-sandbox-env/integration-env.sh status    # component / pool / DART / OpenSandbox health
./scripts/fast-sandbox-env/integration-env.sh pool      # re-apply the pool only
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e   # Python SDK e2e suite against the live stack
./scripts/fast-sandbox-env/integration-env.sh down      # teardown, host left clean
```

After `up`, point any OpenSandbox SDK at `http://127.0.0.1:18080` with the
`OPEN-SANDBOX-API-KEY: fast-sandbox-env` header; sandbox endpoints are
signed `f1.*` header routes served by the gateway at `http://127.0.0.1:18081`.
The installed CLI works against the same stack, e.g.
`OPEN_SANDBOX_DOMAIN=127.0.0.1:18080 OPEN_SANDBOX_API_KEY=fast-sandbox-env osb sandbox list`.

The `up` verify stages are also available as Python SDK e2e tests
(template create → gateway ping → networkpolicy convergence, lifecycle
ops, pause/resume, snapshot round trip):

```bash
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e   # needs uv on PATH
```

or, manually: `cd tests/python && OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$(cat
"$WORK/template-id")" make test-fsb` (requires
`OPENSANDBOX_TEST_FSB_TEMPLATE_ID`; the script stores the id in
`$WORK/template-id`).

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

- **server / ingress / egress** are built from this repository
  (`server/Dockerfile`, `components/ingress`, `components/egress`) and
  kind-loaded; the server and the gateway are installed through
  `manifests/charts/server` and `manifests/charts/ingress-gateway`.
- **execd** is baked into the SandboxTemplate golden image. Templates are
  created through the server's `POST /templates` API (server config
  injects `EXECD` as the build's execd image, kernel stays the builder
  default); `SBX_IMAGE` / `EXECD` select the build inputs.

## Defaults

- **Pool with egress**: `firecracker-egress-pool` (firecracker runtime,
  `poolMin=2`) carries the OpenSandbox egress container in every fastlet
  Pod netns — fast-sandbox profile, `dns+nft` mode — wired through the Sandbox
  Actions channel (`infraComponents` host-process entry +
  `actionHandlers` egress@18080 with the runtime-ready /
  data-plane-ready hooks).
- **P2P by default**: two kind nodes, each running the runtime-agent with
  a node-local DART daemon (peer discovery through the headless `dart`
  Service); the pool spreads one fastlet per node via podAntiAffinity.
  Each node binds its **own** state-root subdirectory
  (`/var/lib/fast-sandbox/control-plane`, `/var/lib/fast-sandbox/worker`)
  at the same container path, so one node's committed image cache never
  satisfies another node's pull — artifact delivery flows
  `cache -> peer -> origin` with the origin fetched ~once per 4MiB block
  cluster-wide on every node's first create. `KIND_SINGLE=1` falls back
  to one node (cache-only).
- **On-demand loading**: the pool has no `warmImages`; the verify
  sandbox's create pulls the golden snapshot set through DART.
  `WARM_IMAGES=1` preheats instead.
- **fast-sandbox @ pinned commit**: env-owned clone at `$WORK/fast-sandbox`,
  cloned on first `up` and checked out at the pinned commit afterwards;
  relocate it with `FSB_DIR`. The source itself has no override — bump the
  commit in `manifests/third-party/fast-sandbox.commit`.

## Layout

```
integration-env.sh          entrypoint: up / down / status / pool
architecture.svg             how the environment works (topology, pipeline,
                             P2P delivery, egress actions channel)
manifests/
  cluster/kind-cluster.yaml    two-node kind cluster, KVM/tun/shm mounts,
                               per-node state-root subdirectories
  pool/firecracker-egress-pool.yaml  SandboxPool: egress attached + P2P spread
```

Everything else comes from the OpenSandbox Helm charts (`manifests/charts`),
rendered with `helm template` from the workdir's values (image tags,
artifact-store endpoint, FastPath endpoint, signing key, NodePorts) and
applied with plain `kubectl apply` — the cluster keeps no helm state:

| Chart | Provides in this environment |
|---|---|
| `base` | `sandbox.fast.io` + `sandbox.opensandbox.io` CRDs, component RBAC, namespaces |
| `fast-sandbox` | all-in-one control plane (reconcilers + FastPath), janitor, firecracker node installer, runtime-agent + DART |
| `server` | lifecycle server: fsb runtime config (rendered `configToml`), NodePort |
| `ingress-gateway` | fsb provider + FastPath + shared signing key, NodePort |

## Stage order (up)

preflight → sysctl → fast-sandbox checkout (pinned commit) → build images
(6 fast-sandbox images via `manifests/release/build-fast-sandbox.sh` +
egress + server + ingress) → XFS StateRoot → kind cluster + node labels
(host ports 18080/18081) → RustFS → charts rendered + applied: base +
fast-sandbox (CRDs, RBAC, control plane, installer, agent) → credentials →
installer/agent roster asserted → charts rendered + applied: server +
ingress gateway → SandboxTemplate golden image
**built through the server `POST /templates` API** → SandboxPool
(fastlet Ready, egress Ready, pool conditions, actions protocol check) →
end-to-end verify (templateId create → signed gateway route → execd
`/ping` → delete) → pause/resume round-trip (POST pause → poll
`Paused` + route released → POST resume → poll `Running` → fresh-route
`/ping` → delete).

Every stage logs to `$WORK/logs/`; failures dump component logs to
`logs/failure-<task>-<ts>.txt`.

## Environment variables (selection)

The RustFS migration replaces the previous `MINIO_*` and `MC_IMAGE` overrides.
Before upgrading an existing environment, run `down` with the previous script.

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
