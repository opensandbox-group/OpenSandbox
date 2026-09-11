# fast-sandbox integration environment

One-command OpenSandbox ecosystem integration environment driven from the
OpenSandbox repository: a two-node kind cluster (KVM passthrough) running
the fast-sandbox Firecracker chain from a `master` checkout, with the
source-built OpenSandbox **egress** sidecar attached to the default pool
and the source-built OpenSandbox **server** (fsb runtime) and **ingress
gateway** wired on top — verified end to end on every `up`.

The last `up` stage creates one sandbox through the server API, reaches
the in-sandbox execd `/ping` through the signed gateway route, and deletes
it again. The environment is all-or-nothing: running fast-sandbox without
the OpenSandbox layers is not a supported shape of this script.

Requires a bare-metal Linux host with KVM (`/dev/kvm`), Docker, Go ≥ 1.25,
cgroup v2, and `sudo` for the XFS StateRoot loop mount and sysctl bump.
Full operational notes (topology, latency characteristics, production
caveats): `fast-sandbox/docs/guides/firecracker-integration-env.md`.

## Usage

```bash
./scripts/fast-sandbox-env/fast-sandbox-env.sh up        # full stack + end-to-end verify
./scripts/fast-sandbox-env/fast-sandbox-env.sh status    # component / pool / DART / OpenSandbox health
./scripts/fast-sandbox-env/fast-sandbox-env.sh pool      # re-apply the pool only
./scripts/fast-sandbox-env/fast-sandbox-env.sh down      # teardown, host left clean
```

After `up`, point any OpenSandbox SDK at `http://127.0.0.1:8080` with the
`OPEN-SANDBOX-API-KEY: fast-sandbox-env` header; sandbox endpoints are
signed `f1.*` header routes served by the gateway at `http://127.0.0.1:8081`.

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
  kind-loaded; manifests live in `manifests/opensandbox/`.
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
- **fast-sandbox @ master**: env-owned clone at `$WORK/fast-sandbox`,
  cloned on first `up` and ff-updated afterwards; override the location
  with `FSB_DIR`, the source with `FSB_GIT_URL` / `FSB_REF`.

## Layout

```
fast-sandbox-env.sh          entrypoint: up / down / status / pool
architecture.svg             how the environment works (topology, pipeline,
                             P2P delivery, egress actions channel)
manifests/
  cluster/kind-cluster.yaml    two-node kind cluster, KVM/tun/shm mounts,
                               per-node state-root subdirectories
  node/firecracker-installer.yaml  firecracker + jailer + arch-aware kernel
                                    onto node hostPath
  node/dart-service.yaml       headless Service backing DART peer discovery
  node/runtime-agent.yaml      per-node agent + DART child (P2P data plane);
                               image rendered from IMG_AGENT
  pool/firecracker-egress-pool.yaml  SandboxPool: egress attached + P2P spread
  opensandbox/server.yaml      lifecycle server: fsb runtime config,
                               NodePort 30880 -> host 8080
  opensandbox/ingress-gateway.yaml   fsb provider + FastPath + shared
                               signing key, NodePort 30881 -> host 8081
```

Canonical fast-sandbox manifests (CRDs, RBAC, control plane, dev route
keys, runtime-environments) are applied directly from the checkout and
are intentionally not duplicated here. Small operational manifests that
define this environment's shape are split by concern above and rendered
(image tags, signing key, ports) into `$WORK/gen` at apply time.

## Stage order (up)

preflight → sysctl → fast-sandbox checkout → build images (6 fast-sandbox
images + egress + server + ingress) → XFS StateRoot → kind cluster + node
labels (host ports 8080/8081) → MinIO → CRDs + control plane →
credentials → firecracker node assets → runtime-agent + DART (roster
asserted) → OpenSandbox server + ingress gateway → SandboxTemplate golden
image **built through the server `POST /templates` API** → SandboxPool
(fastlet Ready, egress Ready, pool conditions, actions protocol check) →
end-to-end verify (templateId create → signed gateway route → execd
`/ping` → delete).

Every stage logs to `$WORK/logs/`; failures dump component logs to
`logs/failure-<task>-<ts>.txt`.

## Environment variables (selection)

| Variable | Default | Meaning |
|---|---|---|
| `WORK` | `$PWD/.fast-sandbox-env` | workspace + logs |
| `FSB_DIR` | `$WORK/fast-sandbox` | fast-sandbox checkout (env-owned clone, created when missing) |
| `KIND_CLUSTER` | `fast-sandbox-integration` | kind cluster name |
| `KIND_SINGLE` | `0` | `1` = single node (cache-only, no peer traffic) |
| `DOCKER_MIRROR` | — | comma list injected as docker.io containerd mirrors |
| `EGRESS_IMAGE` | `docker.io/opensandbox/egress:latest` | egress image tag (built from `components/egress`) |
| `SERVER_HOST_PORT` / `GATEWAY_HOST_PORT` | `8080` / `8081` | host-side publishes for server / gateway (loopback only) |
| `SERVER_IMAGE` | `docker.io/opensandbox/server:env` | server image tag (built from `server/`) |
| `INGRESS_IMAGE` | `docker.io/opensandbox/ingress:env` | ingress image tag (built from `components/ingress`) |
| `IMAGE_AGENT` | `fast-sandbox/firecracker-runtime-agent:dev` | agent image (rendered into the DaemonSet) |
| `POOL_MIN` / `POOL_MAX` | `2` / `2` | pool capacity (auto `1`/`1` when `KIND_SINGLE=1`) |
| `WARM_IMAGES` | `0` | `1` = preheat pool instead of on-demand first-sandbox pull |
| `SBX_IMAGE` / `EXECD` | `alpine:3.19` / `opensandbox/execd:1.1.0` | template build inputs |
| `MINIO_PORT` | `9000` | host-side publish; in-cluster clients always use the container port |
| `MINIO_CONSOLE_PORT` | `9001` | host-side MinIO console publish (human-only; override on port collision) |
| `XFS_STATEROOT` / `XFS_SIZE` | `1` / `24G` | reflink StateRoot on/off, virtual size |
