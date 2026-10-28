#!/usr/bin/env bash
# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# integration-env.sh — one-command fast-sandbox integration environment,
# driven from the OpenSandbox repository.
#
# On a bare-metal Linux KVM host it builds the full stack — fast-sandbox at
# the commit pinned in manifests/third-party/fast-sandbox.commit, a two-node
# kind cluster with KVM passthrough, the OpenSandbox Helm charts (base CRDs,
# fast-sandbox control plane, server, ingress-gateway), a RustFS artifact
# store, and the firecracker-egress-pool SandboxPool with the OpenSandbox
# egress sidecar — and the verify-native matrix then proves the stack end to
# end: sandbox create via the opensandbox CLI, execd /ping through the signed
# gateway route, delete, a pause/resume round trip, a snapshot ->
# restore-from-snapshotId boot, the egress-held credential vault lifecycle
# (OSEP-0022 FastPath relay), and the full MITM CA chain (OSEP-0022 trust
# anchor): a self-signed CA is provisioned as the versioned Secret, the
# server bakes it into the golden image, the pool pins the keypair into
# mitmdump, and a guest HTTPS request proves the template↔pool fingerprint
# pinning plus per-subject credential injection against a CA-signed TLS echo
# endpoint.
#
# All Kubernetes resources come from the charts via `helm template` + plain
# `kubectl apply` (no helm release state); the only env-owned manifests are
# the kind cluster config and the SandboxPool. The verify stages drive the
# server through the published opensandbox CLI ("osb", installed fresh from
# PyPI; OSB_BIN overrides); raw curl remains only for the calls the CLI
# cannot express (networkpolicy replace PUT, metadata merge-patch, snapshot
# re-entry fence) plus the gateway /ping probes.
#
# Usage:
#   ./scripts/fast-sandbox-env/integration-env.sh up             # environment: stack + pool + server/ingress + native template
#   ./scripts/fast-sandbox-env/integration-env.sh verify-native  # verify matrix on the native template (needs up)
#   ./scripts/fast-sandbox-env/integration-env.sh verify-overlaybd  # overlaybd template build + verify matrix (needs up)
#   ./scripts/fast-sandbox-env/integration-env.sh pool           # re-apply the pool only
#   ./scripts/fast-sandbox-env/integration-env.sh status         # component/pool/DART/OpenSandbox health
#   ./scripts/fast-sandbox-env/integration-env.sh down           # teardown, host left clean
#   ./scripts/fast-sandbox-env/integration-env.sh up --auto-clean   # down on failure
#
# Environment overrides (all optional):
#   WORK                 workspace root        (default /data/fast-sandbox-env when /data exists, else $PWD/.fast-sandbox-env)
#   FSB_DIR              fast-sandbox checkout (default $WORK/fast-sandbox; source pinned in manifests/third-party/fast-sandbox.commit)
#   KIND_CLUSTER / KIND_NODE_IMAGE / KIND_RETAIN / KIND_SINGLE
#   DOCKER_MIRROR        comma list injected as docker.io containerd mirrors
#   RUSTFS_PORT / RUSTFS_CONSOLE_PORT / RUSTFS_AK / RUSTFS_SK / RUSTFS_IMAGE / RC_IMAGE / RUSTFS_ENDPOINT
#   IMAGE_<NAME>         fast-sandbox component image tags
#   EGRESS_IMAGE         egress image tag        (default docker.io/opensandbox/egress:latest)
#   SERVER_IMAGE / INGRESS_IMAGE  OpenSandbox server/ingress image tags
#   SERVER_HOST_PORT / GATEWAY_HOST_PORT  host-side publishes (default 18080/18081)
#   WARM_IMAGES=1        preheat pool warmImages (default: on-demand first-sandbox pull)
#   SBX_IMAGE / EXECD    template build inputs   (default opensandbox/fsb-sandbox-golden:latest / opensandbox/execd:latest)
#   POOL_MIN / POOL_MAX  pool capacity           (default 2/2; auto 1/1 when KIND_SINGLE=1)
#   MAX_SANDBOXES_PER_POD per-fastlet sandbox capacity (default 8)
#   XFS_STATEROOT / XFS_SIZE  reflink StateRoot on/off and virtual size
#   OSB_PACKAGE / OSB_BIN  opensandbox CLI for the verify stages (latest from PyPI / preinstalled binary)
#   SKIP_TOOL_INSTALL / SKIP_LEFTOVER_CLEAN / INOTIFY_VALUE
#   IDLE_PAUSE=1     OSEP-0024 idle-pause/wake profile: Redis, server
#                    [automation.idle_pause], gateway activity+wake flags,
#                    idle-pause verify stage
#   IDLE_PAUSE_ROUNDS  auto-pause/wake rounds in the verify stage and idle-e2e (default 3)
#   IDLE_PAUSE_PARK_BUDGET  gateway wake park budget (default 5s; set e.g. 100ms to
#                    force every wake through the marked-503 + client-retry path)
#   REDIS_IMAGE      Redis image for the idle-pause profile (default redis:7-alpine)
#   OCI_STACK        OverlayBD OCI profile, default 1: in-cluster TLS
#                    registry (CA baked into the fsb images), the chart's
#                    runtime.oci node stack, and a full overlaybd verify
#                    pass beside native. Nodes need ublk support. 0 = native-only.
#
# Every stage logs to $WORK/logs/; failures dump component logs to
# logs/failure-<task>-<ts>.txt before exiting (never silently).

# -E (errtrace): stage functions must inherit the ERR trap, or on_error
# (and its failure dump) never fires — every stage runs inside a function.
set -euEo pipefail
ENV_PID=$BASHPID

OSB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="$SCRIPT_DIR/manifests"
# Heavy run-state belongs on a data volume, not the repo/root disk.
if [[ -d /data ]] && [[ -z "${WORK:-}" ]]; then
	WORK="/data/fast-sandbox-env"
else
	WORK="${WORK:-$PWD/.fast-sandbox-env}"
fi
LOGS_DIR="$WORK/logs"
GEN_DIR="$WORK/gen"

# Env-owned fast-sandbox clone; the source is exactly the commit pinned in
# manifests/third-party/fast-sandbox.commit (no ref override — bump the pin).
FSB_DIR="${FSB_DIR:-$WORK/fast-sandbox}"
FSB_REPO="$(sed -n 's/^repo:[[:space:]]*//p' "$OSB_ROOT/manifests/third-party/fast-sandbox.commit")"
FSB_COMMIT="$(sed -n 's/^commit:[[:space:]]*//p' "$OSB_ROOT/manifests/third-party/fast-sandbox.commit")"

KIND_CLUSTER="${KIND_CLUSTER:-fast-sandbox-integration}"
KIND_SINGLE="${KIND_SINGLE:-0}"
KIND_RETAIN="${KIND_RETAIN:-0}"
# Control plane + node runtime (controller/fastpath, firecracker-runtime,
# agent credentials, artifact-store config) ...
NS="opensandbox-system"
# ... while the SandboxPool/Template/Sandbox resources and the fastlet and
# builder Pods they spawn live in the dataplane namespace.
RESOURCE_NS="opensandbox-dataplane"

RUSTFS_IMAGE="${RUSTFS_IMAGE:-rustfs/rustfs:latest}"
RC_IMAGE="${RC_IMAGE:-rustfs/rc:latest}"
RUSTFS_PORT="${RUSTFS_PORT:-19000}"
# In-container ports are fixed (9000 S3 / 9001 console); these vars only move
# the host-side 127.0.0.1 publishes (defaults avoid 9000/9001 collisions).
RUSTFS_CONTAINER_PORT=9000
RUSTFS_CONSOLE_PORT="${RUSTFS_CONSOLE_PORT:-19001}"
RUSTFS_AK="${RUSTFS_AK:-integration-env}"
RUSTFS_SK="${RUSTFS_SK:-integration-env-secret}"
RUSTFS_BUCKET="sandbox-images"
RUSTFS_CONTAINER="${RUSTFS_CONTAINER:-fast-sandbox-env-rustfs}"
RUSTFS_DATA="$WORK/rustfs-data"
RUSTFS_ENDPOINT="${RUSTFS_ENDPOINT:-}"   # auto-derived from the kind network
RC_CONFIG_DIR="$WORK/rc-config"

SBX_IMAGE="${SBX_IMAGE:-opensandbox/fsb-sandbox-golden:latest}"
EXECD="${EXECD:-opensandbox/execd:latest}"
# WARM_IMAGES=1 preheats pool warmImages (keyed by template id); the default
# is the on-demand first-sandbox pull through DART.
WARM_IMAGES="${WARM_IMAGES:-0}"

POOL_NAME="${POOL_NAME:-firecracker-egress-pool}"
MAX_SANDBOXES_PER_POD="${MAX_SANDBOXES_PER_POD:-8}"
if [[ -n "${POOL_MIN:-}" && -n "${POOL_MAX:-}" ]]; then
	:
elif [[ "$KIND_SINGLE" == "1" ]]; then
	# Cache-only topology: a single fastlet is enough (no peer traffic).
	POOL_MIN="${POOL_MIN:-1}"
	POOL_MAX="${POOL_MAX:-1}"
else
	POOL_MIN="${POOL_MIN:-2}"
	POOL_MAX="${POOL_MAX:-2}"
fi

# Image tags (env-overridable per component).
IMG_CONTROLLER="${IMAGE_CONTROLLER:-opensandbox/fsb-controller:dev}"
IMG_FASTLET="${IMAGE_FASTLET:-opensandbox/fsb-fastlet:dev}"
IMG_FASTLET_PROXY="${IMAGE_FASTLET_PROXY:-opensandbox/fsb-fastlet-proxy:dev}"
IMG_JANITOR="${IMAGE_JANITOR:-opensandbox/fsb-janitor:dev}"
IMG_BUILDER="${IMAGE_BUILDER:-opensandbox/fsb-sandboxtemplate-builder:dev}"
# Shared build base for the builder image (built by build-fast-sandbox.sh
# during build_images; rebuilt here only if missing).
IMG_OCI_TOOLS="${IMAGE_OCI_TOOLS:-opensandbox/oci-tools:dev}"
IMG_RUNTIME="${IMAGE_RUNTIME:-opensandbox/fsb-firecracker-runtime:dev}"
IMG_EGRESS="${EGRESS_IMAGE:-docker.io/opensandbox/egress:latest}"

# OverlayBD OCI stack, default on: one cluster serves both template paths.
# Requires node ublk support (CONFIG_BLK_DEV_UBLK). Enables the in-cluster
# TLS registry (CA baked into the fsb images), the chart's runtime.oci node
# stack, the registry credentials/server setting, and the overlaybd verify
# pass. OCI_STACK=0 is the native-only escape hatch.
OCI_STACK="${OCI_STACK:-1}"
OCI_REGISTRY_IMAGE="${OCI_REGISTRY_IMAGE:-registry:2}"
OCI_HTPASSWD_IMAGE="${OCI_HTPASSWD_IMAGE:-httpd:2.4-alpine}"
OCI_REGISTRY_USER="${OCI_REGISTRY_USER:-envregistry}"
OCI_REGISTRY_PASSWORD="${OCI_REGISTRY_PASSWORD:-envregistry-secret}"
OCI_REGISTRY_SECRET="registry-env"
OCI_REGISTRY_REPO="sandbox/templates"
OCI_REGISTRY_HOST="registry.${RESOURCE_NS}.svc.cluster.local"
CA_DIR="$WORK/registry-ca"

# OSEP-0022 MITM trust anchor — always on in this environment: a self-signed
# CA is provisioned as the versioned Secret (ops-runbook shape), the server
# bakes it into the golden-image template, the egress seeds it into mitmdump,
# and a dedicated verify stage proves the whole chain (fingerprint pinning +
# per-subject credential injection over guest HTTPS). A broken chain fails
# closed and observably (MITM addon down on /healthz, template bake, pinning
# check) instead of silently degrading.
MITM_CA_SECRET="${MITM_CA_SECRET:-opensandbox-mitm-ca-v1}"
MITM_CA_DIR="$WORK/mitm-ca"
MITM_CA_FINGERPRINT_FILE="$MITM_CA_DIR/fingerprint"
MITM_CA_FINGERPRINT=""
# TLS echo endpoint (certificate signed by the pinned CA) as a container on
# the kind network (same shape as RustFS): the guest reaches it by IP, the
# vault binding matches by SNI.
MITM_ECHO_CONTAINER="${MITM_ECHO_CONTAINER:-fast-sandbox-env-mitm-echo}"
MITM_ECHO_DOMAIN="${MITM_ECHO_DOMAIN:-echo.test}"
MITM_ECHO_PORT=443
MITM_ECHO_IP=""
MITM_ECHO_NET=""

image_repo() { printf '%s' "${1%:*}"; }
image_tag() { printf '%s' "${1##*:}"; }

# --- OpenSandbox server + ingress gateway (source-built; always deployed) ------

OSB_NS="opensandbox-system"
IMG_SERVER="${SERVER_IMAGE:-docker.io/opensandbox/server:env}"
IMG_INGRESS="${INGRESS_IMAGE:-docker.io/opensandbox/ingress:env}"
FASTPATH_ENDPOINT="fast-sandbox-fastpath.opensandbox-system.svc:9090"
SERVER_API_KEY="fast-sandbox-env"

# OSEP-0024 profile: Redis + server [automation.idle_pause] + gateway activity/wake flags.
IDLE_PAUSE="${IDLE_PAUSE:-0}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
REDIS_RELEASE="idle-pause-redis"
REDIS_DSN="redis://$REDIS_RELEASE.$OSB_NS.svc.cluster.local:6379/0"
# Per-workdir f1.* route-scope signing key: re-applies must keep old routes
# verifiable.
SIGNING_KEY_FILE="$WORK/opensandbox-signing-key"
# Host publish via kind extraPortMappings (loopback only; overridable on
# port collisions).
SERVER_HOST_PORT="${SERVER_HOST_PORT:-18080}"
GATEWAY_HOST_PORT="${GATEWAY_HOST_PORT:-18081}"
SERVER_NODEPORT=30880
GATEWAY_NODEPORT=30881
GATEWAY_ADDRESS="127.0.0.1:$GATEWAY_HOST_PORT"
SERVER_URL="http://127.0.0.1:$SERVER_HOST_PORT"
GATEWAY_URL="http://127.0.0.1:$GATEWAY_HOST_PORT"

# Applied by the firecracker-runtime readiness loop itself (never manually).
KVM_NODE_LABEL="sandbox.fast.io/kvm"
FC_NODE_LABEL="fast-sandbox.io/firecracker-node"

INOTIFY_VALUE="${INOTIFY_VALUE:-8192}"
SYSCTL_BACKUP="$WORK/sysctl-backup"
SKIP_TOOL_INSTALL="${SKIP_TOOL_INSTALL:-0}"
SKIP_LEFTOVER_CLEAN="${SKIP_LEFTOVER_CLEAN:-0}"
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.0}"
HELM_VERSION="${HELM_VERSION:-v3.16.4}"

# opensandbox CLI for the verify stages; installed by ensure_osb.
OSB_PACKAGE="${OSB_PACKAGE:-opensandbox-cli}"
OSB_BIN="${OSB_BIN:-}"
OSB_VENV="$WORK/tools/osb-venv"

# Internal goproxy mirrors can 500 on shared hosts; direct VCS just works there.
FSB_GOPROXY="${FSB_GOPROXY:-direct}"
export GOPROXY="$FSB_GOPROXY"

AUTO_CLEAN=0
ACTION=""

# --- logging / stage machinery --------------------------------------------------

log() { printf '\033[1;34m[fast-sandbox-env]\033[0m %s\n' "$*" | tee -a "$WORK/run.log" >&2; }
die() { printf '\033[1;31m[fast-sandbox-env] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
pass() { printf '\033[1;32m[fast-sandbox-env] PASS\033[0m %s\n' "$*" | tee -a "$WORK/run.log" >&2; }
# fail dumps before exiting: an explicit `exit` does not fire the ERR trap.
fail() { printf '\033[1;31m[fast-sandbox-env] FAIL\033[0m %s\n' "$*" >&2; failure_dump "${STAGE_CUR_LABEL:-$ACTION}" || true; exit 1; }
highlight() { printf '\033[1;36m%s\033[0m\n' "$*"; }

now_ms() { date +%s%N; }   # GNU date (Linux); the script targets a Linux KVM host
ms2s() { awk -v ms="$1" 'BEGIN { printf "%.1f", ms / 1000 }'; }

declare -a STAGE_ORDER=()
declare -a STAGE_MS_LIST=()
STAGE_CUR=""
STAGE_CUR_LABEL=""
STAGE_START_NS=0
STAGE_N=0

stage_begin() {
	STAGE_N=$((STAGE_N + 1))
	STAGE_CUR="$1"
	STAGE_CUR_LABEL="$(printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_')"
	STAGE_START_NS="$(now_ms)"
	printf '\n\033[1;36m==> [%d] %s\033[0m\n' "$STAGE_N" "$1"
}

stage_done() {
	local ms
	ms=$(( ($(now_ms) - STAGE_START_NS) / 1000000 ))
	STAGE_ORDER+=("$STAGE_CUR")
	STAGE_MS_LIST+=("$ms")
	printf '\033[1;32m    OK in %ss\033[0m %s\n' "$(ms2s "$ms")" "${1:-}"
}

run_stage() {
	local description="$1" func="$2"
	shift 2
	stage_begin "$description"
	"$func" "$@"
	stage_done
}

stage_summary() {
	local index name ms total=0
	highlight "== stage timings =="
	printf '  \033[1m%-46s %10s\033[0m\n' "stage" "duration"
	for index in "${!STAGE_ORDER[@]}"; do
		name="${STAGE_ORDER[$index]}"
		ms="${STAGE_MS_LIST[$index]}"
		total=$((total + ms))
		printf '  %-46s %9ss\n' "$name" "$(ms2s "$ms")"
	done
	printf '  \033[1m%-46s %9ss\033[0m\n' "TOTAL" "$(ms2s "$total")"
}

wait_for() { # description attempts command [args...]
	local description="$1" attempts="$2" attempt=0
	shift 2
	while ! "$@" >/dev/null 2>&1; do
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

kubectl_get() { kubectl -n "$RESOURCE_NS" get "$1" -o jsonpath="$2"; }

sudo_() { if [[ "$(id -u)" == 0 ]]; then "$@"; else sudo "$@"; fi; }

# --- helpers ---------------------------------------------------------------------

kind_node() { kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1; }

kind_network() { # docker network of the first node container
	local node
	node="$(kind_node)" || return 1
	docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$node" | tr ' ' '\n' | grep -v '^$' | head -1
}

rc() {
	docker run --rm --network host --user "$(id -u):$(id -g)" -e RC_CONFIG_DIR=/config \
		-e HTTP_PROXY= -e HTTPS_PROXY= -e ALL_PROXY= \
		-e http_proxy= -e https_proxy= -e all_proxy= \
		-v "$RC_CONFIG_DIR:/config" "$RC_IMAGE" "$@"
}

# --- failure dump ------------------------------------------------------------------

on_error() {
	local rc=$? task="$1"
	# Record WHERE it died before anything else: the dump alone cannot
	# distinguish a silent command failure from an environment problem
	# (e.g. the RustFS stage once died in ~1s with zero output).
	log "up failed: task=$task rc=$rc command=${BASH_COMMAND:-unknown} at ${BASH_SOURCE[1]:-?}:${BASH_LINENO[0]:-?}" || true
	# Subshells inherit ERR: propagate the error instead of tearing down
	# main-process resources; ignore SIGPIPE (cancelled runners).
	trap - ERR
	[[ "$BASHPID" == "$ENV_PID" ]] || exit "$rc"
	trap '' PIPE
	# Dump before --auto-clean teardown destroys the evidence.
	failure_dump "$task" || true
	if [[ "$AUTO_CLEAN" == 1 ]]; then
		log "$ACTION failed at $task; --auto-clean: running down" || true
		(down) >/dev/null 2>&1 || true
	fi
	printf '\033[1;31m[fast-sandbox-env] FAILED at %s; dump: %s\033[0m\n' \
		"$task" "$LOGS_DIR/failure-$task-*.txt" >&2 || true
	exit "$rc"
}

failure_dump() {
	local task="$1"
	local dump="$LOGS_DIR/failure-$task-$(date +%s).txt"
	mkdir -p "$LOGS_DIR"
	{
		echo "=== fast-sandbox-env failure: $task ($(date -u +%FT%TZ)) ==="
		env | grep -E '^(KIND_(CLUSTER|SINGLE|RETAIN|NODE_IMAGE)=|FSB_DIR=|SBX_IMAGE=|IMG_[A-Z_]+=|EGRESS_IMAGE=|EXECD=|WORK=|POOL_(NAME|MIN|MAX)=|INGRESS_IMAGE=|WARM_IMAGES=|XFS_(STATEROOT|SIZE)=|RUSTFS_(IMAGE|PORT|CONSOLE_PORT|BUCKET|CONTAINER|DATA|ENDPOINT)=|RC_IMAGE=|OCI_STACK=|MITM_(CA_SECRET|CA_DIR|ECHO_CONTAINER|ECHO_DOMAIN)=)' || true
		echo "--- fast-sandbox checkout ---"
		git -C "$FSB_DIR" rev-parse HEAD 2>&1 || true
		echo "--- kind-create.log (tail) ---"
		tail -40 "$LOGS_DIR/kind-create.log" 2>&1 || true
		echo "--- nodes ---"
		kubectl get nodes -o wide 2>&1 || true
		echo "--- pods ---"
		kubectl get pods -n "$NS" -o wide 2>&1 || true
		echo "--- controller logs (tail) ---"
		kubectl logs -n "$NS" deploy/fast-sandbox-controller --tail=80 2>&1 || true
		echo "--- firecracker-runtime logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime --all-containers --tail=80 2>&1 || true
		# --all-containers: the label selector alone logs only the first
		# container and drops egress. Deep tail: failure loops flood the
		# window and bury the pre-incident history.
		echo "--- fastlet pod logs, every container (deep tail) ---"
		kubectl logs -n "$RESOURCE_NS" -l app=sandbox-fastlet \
			--all-containers --prefix --timestamps --tail=15000 2>&1 || true
		echo "--- fastlet pod logs, previous container instances (crashes) ---"
		for pod in $(kubectl -n "$RESOURCE_NS" get pods -l app=sandbox-fastlet -o name 2>/dev/null); do
			for container in $(kubectl -n "$RESOURCE_NS" get "$pod" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null); do
				prev="$(kubectl -n "$RESOURCE_NS" logs "$pod" -c "$container" --previous --timestamps --tail=200 2>&1 || true)"
				case "$prev" in *"no previous container"*) continue ;; esac
				echo "### ${pod##*/} container=$container (previous)"
				printf '%s\n' "$prev"
			done
		done
		echo "--- builder pods + logs (deep tail: fsb-vm phase + the ublkd daemon errors it reports) ---"
		kubectl get pods -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --tail=2000 2>&1 || true
		if [[ "$OCI_STACK" == 1 ]]; then
			echo "--- OCI registry (pod + logs tail) ---"
			kubectl get pods -n "$RESOURCE_NS" -l app=fast-sandbox-env-registry -o wide 2>&1 || true
			kubectl logs -n "$RESOURCE_NS" -l app=fast-sandbox-env-registry --tail=60 2>&1 || true
		fi
		echo "--- SandboxTemplates (status carries the build failure reason) ---"
		kubectl get sandboxtemplates -n "$RESOURCE_NS" -o yaml 2>&1 || true
		# Sandbox CR states: a pause/snapshot/resume failure shows up as a
		# stuck or failed status condition here, with the phase the poll
		# never reached.
		echo "--- Sandbox CRs (status only) ---"
		kubectl get sandboxes -n "$RESOURCE_NS" -o json 2>/dev/null \
			| jq -c '.items[] | {name: .metadata.name, phase: .status.phase, conditions: .status.conditions, infra: .status.infraComponents}' 2>&1 || true
		# A mount-namespace mismatch (driver staging path invisible to
		# egress) shows up as ENOENT on the egress side only.
		for pod in $(kubectl -n "$RESOURCE_NS" get pods -l app=sandbox-fastlet -o name 2>/dev/null); do
			echo "### ${pod##*/}: snapshot staging visibility"
			echo "-- fastlet view:"
			kubectl -n "$RESOURCE_NS" exec "$pod" -c fastlet -- \
				ls -la /var/lib/fast-sandbox/firecracker/snapshots 2>&1 | tail -8 || true
			echo "-- egress view:"
			kubectl -n "$RESOURCE_NS" exec "$pod" -c egress -- \
				ls -la /var/lib/fast-sandbox/firecracker/snapshots 2>&1 | tail -8 || true
		done
		# MITM CA chain (OSEP-0022) failure scene: the addon-down reason
		# lives on /healthz; seed/confdir visibility separates "Secret never
		# mounted" from "mitmdump failed to start". The echo container and
		# the guest curl stderr are only capturable here (down deletes both).
		echo "--- MITM CA chain diagnostics ---"
		if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$MITM_ECHO_CONTAINER"; then
			echo "-- echo container --"
			docker ps -a --filter "name=$MITM_ECHO_CONTAINER" --format '{{.Names}} {{.Status}}' 2>&1 || true
			docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}' "$MITM_ECHO_CONTAINER" 2>&1 || true
			docker logs "$MITM_ECHO_CONTAINER" --tail=50 2>&1 || true
		fi
		if [[ -d "$MITM_CA_DIR" ]]; then
			echo "-- pinned CA material (names + fingerprint only, never key bytes) --"
			ls -l "$MITM_CA_DIR" 2>&1 || true
			echo "pinned fingerprint: $(cat "$MITM_CA_FINGERPRINT_FILE" 2>/dev/null || echo missing)"
		fi
		if [[ -f "$LOGS_DIR/mitm-ca-openssl.err" ]]; then
			echo "-- openssl stderr (last MITM CA stage failure) --"
			tail -20 "$LOGS_DIR/mitm-ca-openssl.err" 2>&1 || true
		fi
		if [[ -f "$LOGS_DIR/mitm-echo-probe.err" ]]; then
			echo "-- echo endpoint probe stderr --"
			tail -20 "$LOGS_DIR/mitm-echo-probe.err" 2>&1 || true
		fi
		if [[ -s "$MITM_CA_DIR/tls.crt" && -s "$MITM_CA_DIR/echo.crt" ]]; then
			echo "-- certificate extensions (public material) --"
			for cert in tls echo; do
				openssl x509 -in "$MITM_CA_DIR/$cert.crt" -noout -subject -issuer -text 2>/dev/null \
					| grep -E "subject=|issuer=|Basic Constraints|CA:|Key Usage|Alternative|Extended" || true
			done
		fi
		if [[ -f "$LOGS_DIR/mitm-guest-curl.err" ]]; then
			echo "-- last guest curl stderr ($LOGS_DIR/mitm-guest-curl.err) --"
			tail -20 "$LOGS_DIR/mitm-guest-curl.err" 2>&1 || true
		fi
		local mitm_pod
		mitm_pod="$(first_pool_pod 2>/dev/null || true)"
		if [[ -n "$mitm_pod" ]]; then
			echo "-- egress MITM surfaces (pod ${mitm_pod##*/}) --"
			kubectl -n "$RESOURCE_NS" exec "pod/$mitm_pod" -c egress -- \
				curl -fsS -m 5 "http://127.0.0.1:18080/healthz" 2>&1 || true
			echo
			kubectl -n "$RESOURCE_NS" exec "pod/$mitm_pod" -c egress -- \
				curl -fsS -m 5 "http://127.0.0.1:18080/_fastlet/v1/actions/status" 2>&1 || true
			echo
			echo "-- egress seed mount + mitmdump confdir visibility --"
			kubectl -n "$RESOURCE_NS" exec "pod/$mitm_pod" -c egress -- \
				ls -la /var/egress/mitm-ca-seed 2>&1 || true
			kubectl -n "$RESOURCE_NS" exec "pod/$mitm_pod" -c egress -- \
				ls -la /var/lib/mitmproxy/.mitmproxy 2>&1 || true
			kubectl -n "$RESOURCE_NS" exec "pod/$mitm_pod" -c egress -- \
				pgrep -af mitmdump 2>&1 || true
		fi
		if [[ -f "$GEN_DIR/mitm-ca-policy.json" ]]; then
			echo "-- mitm-ca verify sandbox policy --"
			cat "$GEN_DIR/mitm-ca-policy.json" 2>&1 || true
		fi
		echo "--- recent events ($RESOURCE_NS) ---"
		kubectl get events -n "$RESOURCE_NS" --sort-by=.lastTimestamp 2>&1 | tail -30 || true
		echo "--- OpenSandbox pods ($OSB_NS) ---"
		kubectl get pods -n "$OSB_NS" -o wide 2>&1 || true
		echo "--- server logs (tail) ---"
		kubectl logs -n "$OSB_NS" deploy/opensandbox-server --tail=80 2>&1 || true
		echo "--- ingress gateway logs (tail) ---"
		kubectl logs -n "$OSB_NS" deploy/opensandbox-ingress-gateway --tail=80 2>&1 || true
		echo "--- pool ---"
		kubectl get sandboxpool -n "$RESOURCE_NS" -o yaml 2>&1 || true
		echo "--- rustfs docker logs (tail) ---"
		docker logs "$RUSTFS_CONTAINER" --tail=80 2>&1 || true
	} > "$dump" 2>&1 || true
	log "failure dump: $dump"
}

# --- stage: preflight + tooling ------------------------------------------------------

install_release_binary() { # name version url
	local name="$1" version="$2" url="$3" tmp
	log "installing $name $version -> /usr/local/bin/$name"
	tmp="$(mktemp -d)"
	curl -fL --retry 3 -o "$tmp/$name" "$url" \
		|| die "download $name failed ($url); install it manually or retry"
	sudo_ install -m 0755 "$tmp/$name" "/usr/local/bin/$name"
	rm -rf "$tmp"
}

ensure_tool() { # name
	local name="$1"
	if command -v "$name" >/dev/null 2>&1; then
		return 0
	fi
	if [[ "$SKIP_TOOL_INSTALL" == 1 ]]; then
		die "$name is required (SKIP_TOOL_INSTALL=1: install it manually)"
	fi
	case "$name" in
		kind)
			install_release_binary kind "$KIND_VERSION" \
				"https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-amd64"
			;;
		kubectl)
			install_release_binary kubectl "${KUBECTL_VERSION#v}" \
				"https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
			;;
		helm)
			local tmp
			log "installing helm $HELM_VERSION -> /usr/local/bin/helm"
			tmp="$(mktemp -d)"
			curl -fL --retry 3 -o "$tmp/helm.tgz" \
				"https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" \
				|| die "download helm failed; install it manually or retry"
			sudo_ tar -xzf "$tmp/helm.tgz" -C "$tmp" linux-amd64/helm
			sudo_ install -m 0755 "$tmp/linux-amd64/helm" /usr/local/bin/helm
			rm -rf "$tmp"
			;;
		jq)
			log "installing jq via package manager"
			if command -v apt-get >/dev/null 2>&1; then
				sudo_ apt-get install -y jq >/dev/null
			elif command -v yum >/dev/null 2>&1; then
				sudo_ yum install -y jq >/dev/null
			elif command -v apk >/dev/null 2>&1; then
				sudo_ apk add --no-cache jq >/dev/null
			else
				die "no supported package manager to install jq; install it manually"
			fi
			;;
		*)
			die "$name is required; install it manually"
			;;
	esac
	command -v "$name" >/dev/null 2>&1 || die "$name installation failed"
}

ensure_osb() {
	if [[ -n "$OSB_BIN" ]]; then
		[[ -x "$OSB_BIN" ]] || die "OSB_BIN=$OSB_BIN is not executable"
		log "using osb from OSB_BIN: $OSB_BIN"
	else
		if [[ "$SKIP_TOOL_INSTALL" == 1 ]] && [[ ! -x "$OSB_VENV/bin/osb" ]]; then
			die "osb is required (SKIP_TOOL_INSTALL=1: install opensandbox-cli manually or set OSB_BIN)"
		fi
		command -v uv >/dev/null 2>&1 \
			|| die "uv is required to install the opensandbox CLI (or set OSB_BIN)"
		log "installing the latest $OSB_PACKAGE into $OSB_VENV (uv)"
		: > "$LOGS_DIR/osb-install.log"
		rm -rf "$OSB_VENV"
		# Fresh venv at its final path: venv console-script shebangs embed the
		# interpreter's absolute path, so create-elsewhere-and-move breaks them.
		uv venv --python 3.12 "$OSB_VENV" >>"$LOGS_DIR/osb-install.log" 2>&1 \
			|| { tail -n 20 "$LOGS_DIR/osb-install.log" >&2 || true; die "uv venv --python 3.12 failed (full log: $LOGS_DIR/osb-install.log; or set OSB_BIN)"; }
		uv pip install --python "$OSB_VENV/bin/python" --upgrade "$OSB_PACKAGE" \
			>>"$LOGS_DIR/osb-install.log" 2>&1 \
			|| { tail -n 20 "$LOGS_DIR/osb-install.log" >&2 || true; die "uv pip install --upgrade $OSB_PACKAGE failed (index unreachable? full log: $LOGS_DIR/osb-install.log; or set OSB_BIN)"; }
		OSB_BIN="$OSB_VENV/bin/osb"
	fi
	log "osb version: $("$OSB_BIN" --version 2>/dev/null | tail -n1 || echo unknown)"
	pass "opensandbox CLI ready ($OSB_BIN)"
}

host_port_busy() { # port -> 0 when something already listens on 127.0.0.1:<port>
	(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# Pull only when absent locally (*_IMAGE overrides cover private registries).
ensure_image() { # image -> pull only when missing locally
	docker image inspect "$1" >/dev/null 2>&1 && return 0
	docker pull -q "$1" >/dev/null || die "image $1 is not available locally and the pull failed (pre-load it with docker load, or override the *_IMAGE variable)"
}

preflight() {
	[[ "$(uname -s)" == "Linux" ]] \
		|| die "this environment requires a Linux host with KVM (run it on the remote development VM)"
	command -v docker >/dev/null || die "docker is required"
	command -v go >/dev/null || die "go is required (>=1.25, used by make images and gen-registry)"
	command -v openssl >/dev/null || die "openssl is required (signing key + pinned MITM CA generation)"
	ensure_tool kind
	ensure_tool kubectl
	ensure_tool helm
	ensure_tool jq
	docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
	local cgver
	cgver="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || true)"
	log "docker cgroup version=$cgver"
	# kind requires cgroup v2; v1 hosts fail kubepods cgroup creation.
	if [[ "$cgver" == "1" ]]; then
		die "docker cgroup Version is 1; kind requires cgroup v2. Enable it with the kernel cmdline 'systemd.unified_cgroup_hierarchy=1' and reboot"
	fi
	[[ -e /dev/kvm ]] || die "/dev/kvm is missing on this host (KVM required)"
	# Fail fast per heavy-data target instead of ENOSPC mid-run.
	local min_free_kb=$((20 * 1024 * 1024)) avail_kb target xfs_dir
	xfs_dir="${XFS_LOOP_FILE%/*}"
	mkdir -p "$WORK" "$RUSTFS_DATA" "$xfs_dir" 2>/dev/null || true
	local -a targets=("$WORK" "$RUSTFS_DATA" "$xfs_dir")
	for target in "${targets[@]}"; do
		avail_kb="$(df -Pk "$target" 2>/dev/null | awk 'NR==2 {print $4}')"
		[[ "$avail_kb" =~ ^[0-9]+$ ]] || die "cannot determine free disk space on $target"
		if (( avail_kb < min_free_kb )); then
			die "$target has $((avail_kb / 1024 / 1024))G free; at least 20G is required (built images, XFS StateRoot, RustFS artifacts). Free space (docker system prune / old kind clusters) or point WORK / RUSTFS_DATA / XFS_LOOP_FILE at a bigger volume"
		fi
		log "disk headroom: $target has $((avail_kb / 1024 / 1024))G free"
	done
	# Fail fast on busy host ports instead of a cryptic docker/kind bind error.
	local port
	for port in "$RUSTFS_PORT" "$RUSTFS_CONSOLE_PORT" "$SERVER_HOST_PORT" "$GATEWAY_HOST_PORT"; do
		if host_port_busy "$port"; then
			die "127.0.0.1:$port is already in use (check 'ss -ltnp' / 'docker ps'); free it, or set RUSTFS_PORT / RUSTFS_CONSOLE_PORT / SERVER_HOST_PORT / GATEWAY_HOST_PORT"
		fi
	done
	ensure_image "$RUSTFS_IMAGE"
	ensure_image "$RC_IMAGE"
	pass "preflight"
}

sysctl_set() {
	local current
	current="$(sysctl -n fs.inotify.max_user_instances)"
	[[ "$current" -ge "$INOTIFY_VALUE" ]] && return 0
	echo "$current" > "$SYSCTL_BACKUP"
	log "sysctl fs.inotify.max_user_instances: $current -> $INOTIFY_VALUE"
	sudo sysctl -w fs.inotify.max_user_instances="$INOTIFY_VALUE" >/dev/null
}

sysctl_restore() {
	[[ -f "$SYSCTL_BACKUP" ]] || return 0
	local previous
	previous="$(cat "$SYSCTL_BACKUP")"
	log "restoring fs.inotify.max_user_instances -> $previous"
	sudo sysctl -w fs.inotify.max_user_instances="$previous" >/dev/null || true
	rm -f "$SYSCTL_BACKUP"
}

# --- stage: fast-sandbox checkout @ master ---------------------------------------------

# Scratch Go tool run inside the fast-sandbox module (imports
# internal/registryconfig); removed before dirty checks and on down.
FSB_GEN_DIR="$FSB_DIR/.fast-sandbox-env-gen"

ensure_fsb() {
	if [[ ! -d "$FSB_DIR/.git" ]]; then
		log "cloning $FSB_REPO into $FSB_DIR"
		git clone "$FSB_REPO" "$FSB_DIR" || die "clone failed; check network"
	fi
	rm -rf "$FSB_GEN_DIR"
	[[ -z "$(git -C "$FSB_DIR" status --porcelain)" ]] \
		|| die "fast-sandbox checkout at $FSB_DIR has local changes; delete it to re-clone or point FSB_DIR at a clean checkout"
	# Raw SHA fetch needs allow-reachable-sha1-in-want; full fetch otherwise.
	if ! git -C "$FSB_DIR" fetch -q origin "$FSB_COMMIT" 2>/dev/null; then
		git -C "$FSB_DIR" fetch -q origin '+refs/heads/*:refs/remotes/origin/*' \
			|| die "git fetch failed for $FSB_REPO"
	fi
	git -C "$FSB_DIR" rev-parse --verify --quiet "$FSB_COMMIT^{commit}" >/dev/null \
		|| die "pinned commit $FSB_COMMIT is not reachable from $FSB_REPO"
	if [[ "$(git -C "$FSB_DIR" rev-parse HEAD)" != "$FSB_COMMIT" ]]; then
		git -C "$FSB_DIR" clean -ffdx
	fi
	git -C "$FSB_DIR" checkout --force -q "$FSB_COMMIT" \
		|| die "git checkout $FSB_COMMIT failed"
	[[ "$(git -C "$FSB_DIR" rev-parse HEAD)" == "$FSB_COMMIT" ]] \
		|| die "fast-sandbox checkout is not at the pinned commit $FSB_COMMIT"
	log "fast-sandbox @ pinned $(git -C "$FSB_DIR" rev-parse --short HEAD) (manifests/third-party/fast-sandbox.commit)"
	pass "fast-sandbox checkout ready"
}

# --- stage: images ---------------------------------------------------------------------

build_images() {
	log "building fast-sandbox images (pinned commit, firecracker scope) via manifests/release/build-fast-sandbox.sh"
	# Same checkout and defaults as the standalone builder: env and published
	# build paths cannot drift.
	FSB_SRC_DIR="$FSB_DIR" "$OSB_ROOT/manifests/release/build-fast-sandbox.sh" \
		|| die "fast-sandbox image build failed"
	if [[ "$OCI_STACK" == 1 ]]; then
		bake_registry_ca_into_images
	fi
	log "building the OpenSandbox egress image ($IMG_EGRESS)"
	# Context is the repo root (COPYs components/egress + components/internal).
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		-f "$OSB_ROOT/components/egress/Dockerfile" -t "$IMG_EGRESS" "$OSB_ROOT" >/dev/null \
		|| die "egress image build failed"
	log "building the OpenSandbox server image ($IMG_SERVER)"
	# Context is server/ (self-contained Dockerfile).
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		-f "$OSB_ROOT/server/Dockerfile" -t "$IMG_SERVER" "$OSB_ROOT/server" >/dev/null \
		|| die "server image build failed"
	log "building the OpenSandbox ingress image ($IMG_INGRESS)"
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		-f "$OSB_ROOT/components/ingress/Dockerfile" -t "$IMG_INGRESS" "$OSB_ROOT" >/dev/null \
		|| die "ingress image build failed"
	pass "images built (fast-sandbox + OpenSandbox)"
}

# --- stage: XFS StateRoot (reflink CoW per-sandbox rootfs) ------------------------------

XFS_STATEROOT="${XFS_STATEROOT:-1}"
XFS_LOOP_FILE="${XFS_LOOP_FILE:-$WORK/fast-sandbox.img}"
XFS_SIZE="${XFS_SIZE:-24G}"
XFS_MOUNT_POINT="${XFS_MOUNT_POINT:-/var/lib/fast-sandbox}"

ensure_xfsprogs() {
	command -v mkfs.xfs >/dev/null 2>&1 && return 0
	log "installing xfsprogs (mkfs.xfs)"
	if command -v apt-get >/dev/null 2>&1; then
		sudo_ apt-get install -y xfsprogs >/dev/null
	elif command -v yum >/dev/null 2>&1; then
		sudo_ yum install -y xfsprogs >/dev/null
	else
		die "xfsprogs not installed and no supported package manager"
	fi
}

stateroot_xfs_up() {
	[[ "$XFS_STATEROOT" == 1 ]] || {
		sudo_ mkdir -p "$XFS_MOUNT_POINT"
		log "XFS StateRoot disabled (XFS_STATEROOT=0); per-sandbox rootfs pays a full copy"
		return 0
	}
	if findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null | grep -qx xfs; then
		# Leftover from a run killed before teardown: scrub, and recreate the
		# image if still below the agent's min-free floor (it strips the kvm
		# node label and template builds stop scheduling).
		stateroot_scrub
		if (( $(stateroot_free_gib) >= XFS_MIN_FREE_GIB )); then
			pass "XFS StateRoot ready (reflink CoW rootfs, scrubbed)"
			return 0
		fi
		log "StateRoot below ${XFS_MIN_FREE_GIB}GiB free after scrub; recreating the XFS image"
		sudo_ umount "$XFS_MOUNT_POINT" || die "unmount $XFS_MOUNT_POINT failed"
		rm -f "$XFS_LOOP_FILE"
	fi
	ensure_xfsprogs
	if [[ ! -f "$XFS_LOOP_FILE" ]]; then
		log "creating sparse XFS image $XFS_LOOP_FILE (virtual $XFS_SIZE)"
		truncate -s "$XFS_SIZE" "$XFS_LOOP_FILE"
		sudo_ mkfs.xfs -f "$XFS_LOOP_FILE" >/dev/null 2>&1 || die "mkfs.xfs failed on $XFS_LOOP_FILE"
	fi
	sudo_ mkdir -p "$XFS_MOUNT_POINT"
	sudo_ mount -o noatime "$XFS_LOOP_FILE" "$XFS_MOUNT_POINT" \
		|| die "mount $XFS_LOOP_FILE at $XFS_MOUNT_POINT failed (loop support? try XFS_STATEROOT=0)"
	local a b
	a="$XFS_MOUNT_POINT/.reflink-a"
	b="$XFS_MOUNT_POINT/.reflink-b"
	printf 'probe' | sudo_ tee "$a" >/dev/null
	if sudo_ cp --reflink=always "$a" "$b"; then
		sudo_ rm -f "$a" "$b"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
	else
		sudo_ rm -f "$a" "$b"
		die "reflink probe failed on $XFS_MOUNT_POINT (CoW rootfs would not work)"
	fi
}

# Purge per-node runtime caches in the mounted StateRoot (shared by down()
# and the reuse branch of stateroot_xfs_up); snapshots/ leaks across runs.
stateroot_scrub() {
	local node_dir
	for node_dir in "$XFS_MOUNT_POINT"/*/; do
		[[ -d "${node_dir}firecracker" ]] || continue
		log "purging node runtime cache under ${node_dir%/}"
		sudo_ rm -rf "${node_dir}firecracker/images" \
			"${node_dir}firecracker/agent" \
			"${node_dir}firecracker/jails" \
			"${node_dir}firecracker/cache" \
			"${node_dir}firecracker/snapshots" 2>/dev/null || true
	done
}

# Free space on the mounted StateRoot, in GiB (GNU df).
stateroot_free_gib() {
	df -BG --output=avail "$XFS_MOUNT_POINT" 2>/dev/null | tail -1 | tr -dc '0-9' || echo 0
}
XFS_MIN_FREE_GIB="${XFS_MIN_FREE_GIB:-12}"

stateroot_xfs_down() {
	[[ "$XFS_STATEROOT" == 1 ]] || return 0
	if findmnt -no SOURCE "$XFS_MOUNT_POINT" 2>/dev/null | grep -q "$(basename "$XFS_LOOP_FILE")"; then
		log "unmounting XFS StateRoot $XFS_MOUNT_POINT"
		sudo_ umount "$XFS_MOUNT_POINT"
	fi
	rm -f "$XFS_LOOP_FILE"
}

# --- stage: kind cluster -----------------------------------------------------------------

render_kind_config() { # > $GEN_DIR/kind-cluster.yaml
	local src="$MANIFESTS_DIR/cluster/kind-cluster.yaml" out="$GEN_DIR/kind-cluster.yaml"
	mkdir -p "$GEN_DIR"
	cp "$src" "$out"
	# OCI_STACK=1 binds the host /dev into every node (FIRST extraMount, so
	# the private /dev/shm mount below it shadows the host shm again): the
	# kernel creates ublk device nodes on the host devtmpfs at add time and
	# this kernel registers no block major, so a static node list cannot
	# track them.
	if [[ "$OCI_STACK" == 1 ]]; then
		awk '
			/^  # @UBLK_DEV_MOUNT@$/ {
				print "  - hostPath: /dev"
				print "    containerPath: /dev"
				next }
			{ print }
		' "$out" > "$out.tmp" && mv "$out.tmp" "$out"
	else
		grep -v '^  # @UBLK_DEV_MOUNT@$' "$out" > "$out.tmp" && mv "$out.tmp" "$out"
	fi
	if [[ "$KIND_SINGLE" == "1" ]]; then
		# Strip the worker node: cache-only topology, no peer traffic.
		awk '/^- role: worker/ {exit} {print}' "$out" > "$out.tmp" && mv "$out.tmp" "$out"
		log "single-node topology (KIND_SINGLE=1: no worker, no peer traffic)"
	fi
	if [[ -n "${DOCKER_MIRROR:-}" ]]; then
		# Host-specific, hence opt-in; one TOML endpoint ARRAY (repeated
		# endpoint keys would override each other).
		local block_file="$GEN_DIR/docker-mirror-block.yaml" mirror trimmed endpoints=""
		IFS=',' read -ra mirrors <<<"$DOCKER_MIRROR"
		for mirror in "${mirrors[@]}"; do
			trimmed="$(printf '%s' "$mirror" | tr -d '[:space:]')"
			[[ -n "$trimmed" ]] || continue
			[[ -z "$endpoints" ]] && endpoints="\"$trimmed\"" || endpoints+=", \"$trimmed\""
		done
		[[ -n "$endpoints" ]] || die "DOCKER_MIRROR produced no usable endpoints"
		{
			echo 'containerdConfigPatches:'
			echo '- |-'
			echo '  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."docker.io"]'
			echo "    endpoint = [$endpoints]"
		} > "$block_file"
		awk -v block_file="$block_file" '
			NR == FNR { block = block $0 "\n"; next }
			/^nodes:/ && !done { printf "%s", block; done = 1 }
			{ print }
		' "$block_file" "$out" > "$out.tmp" && mv "$out.tmp" "$out"
		rm -f "$block_file"
		log "docker.io containerd mirrors injected: $endpoints"
	fi
	# extraPortMappings publish the server/gateway on the host; they exist
	# only at cluster creation, so a cluster reused without them stays
	# cluster-internal.
	local ports_file="$GEN_DIR/osb-ports-block.yaml"
	{
		echo '  extraPortMappings:'
		echo "  - containerPort: $SERVER_NODEPORT"
		echo "    hostPort: $SERVER_HOST_PORT"
		echo '    listenAddress: 127.0.0.1'
		echo '    protocol: TCP'
		echo "  - containerPort: $GATEWAY_NODEPORT"
		echo "    hostPort: $GATEWAY_HOST_PORT"
		echo '    listenAddress: 127.0.0.1'
		echo '    protocol: TCP'
	} > "$ports_file"
	awk -v block_file="$ports_file" '
		NR == FNR { block = block $0 "\n"; next }
		/^- role: control-plane/ && !done { print; printf "%s", block; done = 1; next }
		{ print }
	' "$ports_file" "$out" > "$out.tmp" && mv "$out.tmp" "$out"
	rm -f "$ports_file"
	log "server on 127.0.0.1:$SERVER_HOST_PORT, gateway on 127.0.0.1:$GATEWAY_HOST_PORT (NodePorts $SERVER_NODEPORT/$GATEWAY_NODEPORT)"
}

kind_up() {
	local create_args=() kind_config="$GEN_DIR/kind-cluster.yaml"
	[[ -f "$kind_config" ]] || die "kind config not rendered ($kind_config)"
	[[ "$KIND_RETAIN" == 1 ]] && create_args+=(--retain)
	if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]]; then
		log "cluster $KIND_CLUSTER already exists; reusing (run down first for a clean rebuild)"
	else
		if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
			log "pulling kind node image $KIND_NODE_IMAGE (this can take minutes)"
			ensure_image "$KIND_NODE_IMAGE" || die "kind node image unavailable locally and pull failed (KIND_NODE_IMAGE=$KIND_NODE_IMAGE)"
			create_args+=(--image "$KIND_NODE_IMAGE")
		else
			log "creating cluster (pulling kindest/node may take minutes; set KIND_NODE_IMAGE to a mirror if it fails)"
		fi
		kind create cluster --name "$KIND_CLUSTER" \
			${create_args+"${create_args[@]}"} --config "$kind_config" > "$LOGS_DIR/kind-create.log" 2>&1 || {
			local rc=$?
			tail -40 "$LOGS_DIR/kind-create.log" >&2 || true
			log "kind create failed (full log: $LOGS_DIR/kind-create.log)" || true
			return "$rc"	# through ERR, so diagnostics + --auto-clean run
		}
		pass "kind cluster created"
	fi
	local node
	for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
		docker exec "$node" sh -c 'test -e /dev/kvm' || die "KVM not visible inside the kind node container $node"
		log "node $node: /dev/kvm visible"
	done
	if [[ "$KIND_SINGLE" != "1" ]]; then
		# Untaint the control plane: agent/fastlet/builder must schedule on
		# BOTH nodes for P2P peer traffic.
		kubectl taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true
		log "control-plane taint removed (both nodes schedulable for the P2P topology)"
	fi
	pass "kind cluster ready (KVM passthrough + labels on every node)"
}

# --- stage: RustFS + credentials ------------------------------------------------------------

rustfs_up() {
	docker rm -f "$RUSTFS_CONTAINER" >/dev/null 2>&1 || true
	# RustFS runs as UID/GID 10001 and owns the bind-mounted store.
	sudo_ rm -rf "$RUSTFS_DATA"
	mkdir -p "$RUSTFS_DATA"
	sudo_ chown -R 10001:10001 "$RUSTFS_DATA"
	mkdir -p "$RC_CONFIG_DIR"
	chmod 700 "$RC_CONFIG_DIR"
	local net
	net="$(kind_network)"
	# On the kind network: pods reach the container IP directly (no
	# docker-proxy/hairpin issues); 127.0.0.1 publishes serve the host.
	docker run -d --name "$RUSTFS_CONTAINER" --network "$net" \
		-p 127.0.0.1:"$RUSTFS_PORT":"$RUSTFS_CONTAINER_PORT" -p 127.0.0.1:"$RUSTFS_CONSOLE_PORT":9001 \
		-e RUSTFS_ACCESS_KEY="$RUSTFS_AK" -e RUSTFS_SECRET_KEY="$RUSTFS_SK" \
		-e RUSTFS_ADDRESS=":$RUSTFS_CONTAINER_PORT" \
		-e RUSTFS_CONSOLE_ADDRESS=:9001 -e RUSTFS_CONSOLE_ENABLE=true \
		-v "$RUSTFS_DATA:/data" \
		"$RUSTFS_IMAGE" >/dev/null
	local attempt
	for attempt in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$RUSTFS_PORT/health" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "RustFS did not become healthy"
	done
	rc alias set chain "http://127.0.0.1:$RUSTFS_PORT" "$RUSTFS_AK" "$RUSTFS_SK" >/dev/null 2>&1 \
		|| die "could not configure the RustFS rc alias"
	for attempt in $(seq 1 30); do
		if rc ls chain/ >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "RustFS S3 API did not become ready for authenticated bucket listing"
	done
	rc mb "chain/$RUSTFS_BUCKET" >/dev/null
	pass "RustFS up (bucket=$RUSTFS_BUCKET)"
}

resolve_rustfs_endpoint() {
	if [[ -n "$RUSTFS_ENDPOINT" ]]; then
		log "RustFS endpoint (env): $RUSTFS_ENDPOINT"
	else
		local net ips ip
		net="$(kind_network)"
		ips="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}} {{end}}' "$RUSTFS_CONTAINER")"
		ip="$(printf '%s' "$ips" | tr ' ' '\n' | grep -A1 -x "^$net$" | tail -1)"
		[[ -n "$ip" ]] || die "could not find the RustFS IP on network $net (inspect: $ips)"
		# Container port, not the host publish: kind-network clients hit the
		# container IP directly.
		RUSTFS_ENDPOINT="http://$ip:$RUSTFS_CONTAINER_PORT"
		log "RustFS endpoint (kind network IP): $RUSTFS_ENDPOINT"
	fi
	local net
	net="$(kind_network)"
	docker run --rm --network "$net" --user "$(id -u):$(id -g)" -e RC_CONFIG_DIR=/config \
		-e HTTP_PROXY= -e HTTPS_PROXY= -e ALL_PROXY= \
		-e http_proxy= -e https_proxy= -e all_proxy= \
		-v "$RC_CONFIG_DIR:/config" "$RC_IMAGE" alias set kind-rustfs \
		"$RUSTFS_ENDPOINT" "$RUSTFS_AK" "$RUSTFS_SK" >/dev/null 2>&1 \
		|| die "RustFS unreachable from the kind network at $RUSTFS_ENDPOINT (override RUSTFS_ENDPOINT)"
	docker run --rm --network "$net" --user "$(id -u):$(id -g)" -e RC_CONFIG_DIR=/config \
		-e HTTP_PROXY= -e HTTPS_PROXY= -e ALL_PROXY= \
		-e http_proxy= -e https_proxy= -e all_proxy= \
		-v "$RC_CONFIG_DIR:/config" "$RC_IMAGE" ls "kind-rustfs/$RUSTFS_BUCKET" >/dev/null 2>&1 \
		|| die "RustFS authenticated bucket access failed from the kind network at $RUSTFS_ENDPOINT"
	pass "RustFS reachable from the kind network"
}

# gen_registry builds the agent registry JSON via fast-sandbox's
# registryconfig package; the write pair covers checkpoint publication.
gen_registry() { # host username password endpoint [write-username write-password] > registry.json
	mkdir -p "$FSB_GEN_DIR"
	cat > "$FSB_GEN_DIR/gen-registry.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"fast-sandbox/internal/registryconfig"
)

func main() {
	if len(os.Args) != 5 && len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: gen-registry <host> <username> <password> <endpoint> [write-username write-password]")
		os.Exit(1)
	}
	credential := registryconfig.Credential{
		Host: os.Args[1], Username: os.Args[2], Password: os.Args[3], Endpoint: os.Args[4],
	}
	if len(os.Args) == 7 {
		// Optional publish (write) pair: empty keeps the store read-only.
		credential.WriteUsername, credential.WritePassword = os.Args[5], os.Args[6]
	}
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{credential})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	payload, err := compiled.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(payload)
}
EOF
	(cd "$FSB_DIR" && GOTOOLCHAIN=local go run .fast-sandbox-env-gen/gen-registry.go "$@")
}

credentials_up() {
	# Namespaces exist even after a partial up (resume support).
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl create namespace "$RESOURCE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local host
	host="${RUSTFS_ENDPOINT#http://}"
	host="${host#https://}"
	# Publish credentials for the builder Pod (dataplane namespace).
	kubectl -n "$RESOURCE_NS" create secret generic sandbox-oss-credentials \
		--from-literal=accessKeyId="$RUSTFS_AK" \
		--from-literal=secretAccessKey="$RUSTFS_SK" \
		--from-literal=endpoint="$RUSTFS_ENDPOINT" \
		--from-literal=region=us-east-1 \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Agent pull+publish credentials (the write pair covers checkpoints).
	gen_registry "$host" "$RUSTFS_AK" "$RUSTFS_SK" "$RUSTFS_ENDPOINT" "$RUSTFS_AK" "$RUSTFS_SK" \
		> "$WORK/agent-registry.json"
	jq -e '.credentials[0].writeUsername' "$WORK/agent-registry.json" >/dev/null \
		|| die "generated agent registry carries no write credential"
	kubectl -n "$NS" create secret generic fast-sandbox-agent-registry \
		--from-file=registry.json="$WORK/agent-registry.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the fastlet; the artifact-store ConfigMap is
	# rendered by charts/fast-sandbox at install time.
	kubectl -n "$RESOURCE_NS" create secret docker-registry registry-rustfs \
		--docker-server="$host" --docker-username="$RUSTFS_AK" --docker-password="$RUSTFS_SK" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local registries_yaml="registries:
  - host: $host
    secretRef:
      name: registry-rustfs
"
	if [[ "$OCI_STACK" == 1 ]]; then
		# OverlayBD entries: repositoryPrefix scopes the account to the
		# sandbox/* subtree (templates + checkpoints share one repository
		# account — the node ublkd has a service-wide credential context).
		registries_yaml+="  - host: $OCI_REGISTRY_HOST
    repositoryPrefix: ${OCI_REGISTRY_REPO%%/*}
    secretRef:
      name: $OCI_REGISTRY_SECRET
"
		kubectl -n "$RESOURCE_NS" create secret docker-registry "$OCI_REGISTRY_SECRET" \
			--docker-server="$OCI_REGISTRY_HOST" \
			--docker-username="$OCI_REGISTRY_USER" \
			--docker-password="$OCI_REGISTRY_PASSWORD" \
			--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	fi
	kubectl -n "$RESOURCE_NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=$registries_yaml" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written (publish/pull)"
}

# --- stage: MITM CA material (OSEP-0022 trust anchor) -------------------------

# mitm_ca_fingerprint <cert.pem> -> lowercase hex SHA-256 over the DER — the
# exact fingerprint form the egress /healthz and the template manifest
# lineage expose.
mitm_ca_fingerprint() {
	openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null | sed 's/^.*=//' | tr -d ':' | tr 'A-Z' 'a-z'
}

load_mitm_ca_fingerprint() {
	if [[ -z "$MITM_CA_FINGERPRINT" && -s "$MITM_CA_FINGERPRINT_FILE" ]]; then
		MITM_CA_FINGERPRINT="$(cat "$MITM_CA_FINGERPRINT_FILE")"
	fi
}

# Cross-invocation companion of load_mitm_ca_fingerprint: the echo endpoint's
# kind-network IP is a docker-container fact, re-derived (not persisted) so a
# verify action sees the live container rather than a stale address.
load_mitm_echo_endpoint() {
	MITM_ECHO_NET="$(kind_network 2>/dev/null || true)"
	MITM_ECHO_IP="$(docker inspect -f \
		'{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' \
		"$MITM_ECHO_CONTAINER" 2>/dev/null || true)"
	[[ -n "$MITM_ECHO_IP" ]] \
		|| die "TLS echo container $MITM_ECHO_CONTAINER is not running (run up first; mitm_echo_up starts it)"
}

mitm_ca_up() {
	local stored_fp openssl_err
	openssl_err="$LOGS_DIR/mitm-ca-openssl.err"
	mkdir -p "$MITM_CA_DIR"
	# Toolchain evidence: the chain guard depends on the host openssl, so a
	# failure must always carry the version + stderr (never swallowed).
	openssl version >> "$LOGS_DIR/environment.txt" 2>&1 || true
	log "openssl: $(command -v openssl) ($(openssl version 2>&1))"
	# Fresh material every run: the env owns no CA state worth carrying
	# across runs (the template that bakes it is rebuilt each time anyway);
	# down() removes the directory so a stale CA can never resurface.
	rm -f "$MITM_CA_DIR"/tls.crt "$MITM_CA_DIR"/tls.key "$MITM_CA_DIR"/tls.srl \
		"$MITM_CA_DIR"/ca.cnf "$MITM_CA_DIR"/echo.crt "$MITM_CA_DIR"/echo.key "$MITM_CA_DIR"/echo.csr "$MITM_CA_DIR"/echo.ext \
		"$MITM_CA_FINGERPRINT_FILE"
	# Versioned, immutable-by-convention CA (the v1 of opensandbox-mitm-ca-*):
	# a real signing CA (keyCertSign), what the egress seed validator demands.
	# Extensions ride an explicit config file (-config + -extensions), not
	# -addext: the runner-family openssl builds seen in CI (1.1.1k-FIPS)
	# have verifier quirks, so generation sticks to the most portable form
	# (config sections work identically from 1.0.2 through 3.x). Every
	# openssl call captures stderr into $openssl_err so a failure carries
	# the toolchain's own diagnosis, not just our message.
	cat > "$MITM_CA_DIR/ca.cnf" <<CNF
[req]
distinguished_name = dn
prompt = no

[dn]
CN = $MITM_CA_SECRET

[v3_ca]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always, issuer

[v3_echo]
subjectAltName = DNS:$MITM_ECHO_DOMAIN
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid, issuer
CNF
	# -days 30 (not 2): the verify actions support running against an
	# environment created earlier, so short-lived material would time-bomb
	# every TLS handshake there. up regenerates fresh material per run
	# anyway; the window only has to outlive delayed verify runs.
	if ! openssl req -x509 -new -newkey rsa:2048 -nodes -days 30 \
		-keyout "$MITM_CA_DIR/tls.key" -out "$MITM_CA_DIR/tls.crt" \
		-config "$MITM_CA_DIR/ca.cnf" -extensions v3_ca 2>"$openssl_err"; then
		die "pinned MITM CA generation failed (openssl $(openssl version 2>&1)): $(tail -3 "$openssl_err" 2>/dev/null | tr '\n' ' ')"
	fi
	# Generator self-check: a build that silently drops the CA constraint
	# would seed cleanly and fail every downstream TLS handshake with an
	# opaque issuer-lookup error — refuse it here, with the evidence.
	if ! openssl x509 -in "$MITM_CA_DIR/tls.crt" -noout -text 2>/dev/null | grep -q "CA:TRUE"; then
		die "generated CA certificate lacks BasicConstraints CA:TRUE (openssl $(openssl version 2>&1) misbehaved): $(openssl x509 -in "$MITM_CA_DIR/tls.crt" -noout -text 2>/dev/null | grep -A1 -E 'Basic Constraints|Key Usage' | tr '\n' ' ')"
	fi
	log "pinned CA extensions: $(openssl x509 -in "$MITM_CA_DIR/tls.crt" -noout -text 2>/dev/null | grep -A1 -E 'Basic Constraints|Key Usage' | tr '\n' ';' | tr -s ' ')"
	MITM_CA_FINGERPRINT="$(mitm_ca_fingerprint "$MITM_CA_DIR/tls.crt")"
	[[ "$MITM_CA_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]] \
		|| die "generated MITM CA fingerprint is malformed: $MITM_CA_FINGERPRINT"
	printf '%s' "$MITM_CA_FINGERPRINT" > "$MITM_CA_FINGERPRINT_FILE"
	# Leaf certificate for the TLS echo endpoint, signed by the pinned CA —
	# both trust legs of the chain test terminate at it (the guest trusts it
	# through the MITM-signed certificate, mitmdump trusts it upstream).
	if ! openssl req -new -newkey rsa:2048 -nodes \
		-keyout "$MITM_CA_DIR/echo.key" -out "$MITM_CA_DIR/echo.csr" \
		-subj "/CN=$MITM_ECHO_DOMAIN" 2>>"$openssl_err"; then
		die "echo certificate request generation failed: $(tail -3 "$openssl_err" 2>/dev/null | tr '\n' ' ')"
	fi
	if ! openssl x509 -req -in "$MITM_CA_DIR/echo.csr" \
		-CA "$MITM_CA_DIR/tls.crt" -CAkey "$MITM_CA_DIR/tls.key" -CAcreateserial \
		-out "$MITM_CA_DIR/echo.crt" -days 30 \
		-extfile "$MITM_CA_DIR/ca.cnf" -extensions v3_echo 2>>"$openssl_err"; then
		die "echo certificate signing failed: $(tail -3 "$openssl_err" 2>/dev/null | tr '\n' ' ')"
	fi
	# Structural chain check (issuer == CA subject): deliberately NOT
	# `openssl verify`, whose trust-store behavior diverges across the
	# openssl builds seen on runners (1.1.1k-FIPS failed a textbook-valid
	# chain with error 20). The chain itself is genuinely verified later at
	# runtime — the guest curls without -k against the baked CA, and
	# mitmdump verifies the upstream against the pinned CA — so this early
	# check only needs to catch a mis-signed leaf, which a DN comparison
	# does on every openssl version.
	local leaf_issuer ca_subject
	leaf_issuer="$(openssl x509 -in "$MITM_CA_DIR/echo.crt" -noout -issuer 2>>"$openssl_err" | sed 's/^issuer=//')"
	ca_subject="$(openssl x509 -in "$MITM_CA_DIR/tls.crt" -noout -subject 2>>"$openssl_err" | sed 's/^subject=//')"
	[[ -n "$leaf_issuer" && "$leaf_issuer" == "$ca_subject" ]] \
		|| die "echo certificate issuer does not match the pinned CA subject: issuer='$leaf_issuer' subject='$ca_subject'"
	# Ops-runbook distribution, collapsed into one namespace here (the pool
	# and template-build namespaces coincide): the keypair entries feed the
	# egress seed mount, the certificate-only entry (ca.crt) feeds the server
	# template bake. A real deployment keeps the certificate-only copy in the
	# template namespace and never carries the key.
	kubectl -n "$RESOURCE_NS" create secret generic "$MITM_CA_SECRET" \
		--from-file=tls.crt="$MITM_CA_DIR/tls.crt" \
		--from-file=tls.key="$MITM_CA_DIR/tls.key" \
		--from-file=ca.crt="$MITM_CA_DIR/tls.crt" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Content, not name: the stored certificate must fingerprint identically
	# (the same-named-copy-wrong-content drift the design calls out).
	stored_fp="$(kubectl -n "$RESOURCE_NS" get secret "$MITM_CA_SECRET" \
		-o jsonpath='{.data.tls\.crt}' | base64 -d | mitm_ca_fingerprint /dev/stdin)"
	[[ "$stored_fp" == "$MITM_CA_FINGERPRINT" ]] \
		|| die "Secret $MITM_CA_SECRET certificate fingerprint mismatch: stored=$stored_fp pinned=$MITM_CA_FINGERPRINT"
	pass "pinned MITM CA provisioned ($MITM_CA_SECRET, fingerprint ${MITM_CA_FINGERPRINT:0:16}…)"
}

# --- stage: TLS echo endpoint (certificate signed by the pinned CA) -----------

# The echo reflects the request headers it observed as the response body —
# the injected credential header must appear there, which is the per-subject
# injection assertion. Runs as a plain container on the kind network (same
# shape as RustFS): reuses the locally built egress image (python3) so no
# new image dependency is introduced.
mitm_echo_up() {
	local net script probe
	net="$(kind_network)"
	mkdir -p "$GEN_DIR"
	script="$GEN_DIR/mitm-echo-server.py"
	cat > "$script" <<'PY'
import http.server
import json
import ssl
import sys

port, cert, key = int(sys.argv[1]), sys.argv[2], sys.argv[3]


class Handler(http.server.BaseHTTPRequestHandler):
    def _echo(self):
        body = json.dumps({k.lower(): v for k, v in self.headers.items()}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = _echo
    do_POST = _echo

    def log_message(self, *args):
        pass


httpd = http.server.ThreadingHTTPServer(("0.0.0.0", port), Handler)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
httpd.serve_forever()
PY
	docker rm -f "$MITM_ECHO_CONTAINER" >/dev/null 2>&1 || true
	# --entrypoint: the egress image's supervisor ENTRYPOINT must not wrap
	# this process; root (image default) binds :443.
	docker run -d --name "$MITM_ECHO_CONTAINER" --network "$net" \
		--entrypoint python3 \
		-v "$MITM_CA_DIR/echo.crt":/etc/mitm-echo/tls.crt:ro \
		-v "$MITM_CA_DIR/echo.key":/etc/mitm-echo/tls.key:ro \
		-v "$script":/srv/mitm-echo-server.py:ro \
		"$IMG_EGRESS" /srv/mitm-echo-server.py "$MITM_ECHO_PORT" \
		/etc/mitm-echo/tls.crt /etc/mitm-echo/tls.key >/dev/null \
		|| die "TLS echo container failed to start"
	MITM_ECHO_IP="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' "$MITM_ECHO_CONTAINER")"
	MITM_ECHO_NET="$net"
	[[ -n "$MITM_ECHO_IP" ]] || die "could not resolve the echo container IP on network $net"
	wait_for "TLS echo endpoint answering on $MITM_ECHO_IP:$MITM_ECHO_PORT" 30 mitm_echo_ready
	probe="$(mitm_echo_headers 'X-Mitm-Echo-Probe: env-up')"
	# Whitespace-tolerant: the body is python json.dumps output (": " separators).
	printf '%s' "$probe" | grep -Eq '"x-mitm-echo-probe":[[:space:]]*"env-up"' \
		|| die "echo endpoint did not reflect headers: $probe"
	pass "TLS echo endpoint up ($MITM_ECHO_DOMAIN @ $MITM_ECHO_IP:$MITM_ECHO_PORT, cert signed by the pinned CA)"
}

mitm_echo_ready() {
	[[ -n "$MITM_ECHO_IP" && -n "$MITM_ECHO_NET" ]] || return 1
	mitm_echo_curl >/dev/null 2>&1
}

# mitm_echo_curl [extra-header]: HTTPS probe of the echo endpoint from a
# container ON the kind network (the resolve_rustfs_endpoint precedent) —
# the host->docker-bridge route is not a path this environment relies on
# anywhere else. Runs the locally built egress image's curl, verifying the
# chain against the pinned CA (probe stderr lands in
# $LOGS_DIR/mitm-echo-probe.err for the failure dump).
mitm_echo_curl() {
	local extra=()
	[[ -n "${1:-}" ]] && extra=(-H "$1")
	docker run --rm --network "$MITM_ECHO_NET" \
		--entrypoint curl \
		-v "$MITM_CA_DIR/tls.crt":/probe/ca.crt:ro \
		-e HOME=/tmp \
		"$IMG_EGRESS" \
		"${extra[@]}" \
		-fsS -m 10 --noproxy '*' --cacert /probe/ca.crt \
		--resolve "$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT:$MITM_ECHO_IP" \
		"https://$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT/echo" 2>"$LOGS_DIR/mitm-echo-probe.err"
}

# mitm_echo_headers [extra-header] -> reflected request headers (JSON body).
mitm_echo_headers() {
	mitm_echo_curl "${1:-}"
}

# --- stage: control plane -------------------------------------------------------------------

# helm is only a renderer; the cluster keeps no helm release state, so
# re-runs stay plain idempotent `kubectl apply`.
helm_render() { # release chart ns out [set-args...]
	local release="$1" chart="$2" ns="$3" out="$4"
	shift 4
	helm template "$release" "$chart" --namespace "$ns" "$@" > "$out" \
		|| die "helm template $chart failed"
}

apply_ns() { # ns -> ensure the namespace exists (idempotent)
	kubectl create namespace "$1" --dry-run=client -o yaml 2>/dev/null |
		kubectl apply -f - >/dev/null
}

control_plane_up() {
	# Charts are the source of truth (CRDs + RBAC + control plane), replacing
	# the fast-sandbox checkout's kustomize applies and env-owned node manifests.
	apply_ns "$NS"
	helm_render fsb-base "$OSB_ROOT/manifests/charts/base" "$NS" "$GEN_DIR/fsb-base.yaml"
	kubectl apply -f "$GEN_DIR/fsb-base.yaml" >/dev/null
	helm_render fast-sandbox "$OSB_ROOT/manifests/charts/fast-sandbox" "$NS" \
		"$GEN_DIR/fast-sandbox.yaml" \
		--set controller.image.repository="$(image_repo "$IMG_CONTROLLER")" \
		--set controller.image.tag="$(image_tag "$IMG_CONTROLLER")" \
		--set controller.sandboxtemplateBuilderImage="$IMG_BUILDER" \
		--set controller.fastletProxyImage="$IMG_FASTLET_PROXY" \
		--set janitor.image.repository="$(image_repo "$IMG_JANITOR")" \
		--set janitor.image.tag="$(image_tag "$IMG_JANITOR")" \
		--set runtime.image.repository="$(image_repo "$IMG_RUNTIME")" \
		--set runtime.image.tag="$(image_tag "$IMG_RUNTIME")" \
		--set artifactStore.store="s3://$RUSTFS_BUCKET/publish" \
		--set artifactStore.endpoint="$RUSTFS_ENDPOINT" \
		--set runtime.oci.enabled="$([[ "$OCI_STACK" == 1 ]] && echo true || echo false)"
	kubectl apply -f "$GEN_DIR/fast-sandbox.yaml" >/dev/null
	local image
	for image in "$IMG_CONTROLLER" "$IMG_FASTLET" "$IMG_FASTLET_PROXY" \
		"$IMG_JANITOR" "$IMG_RUNTIME" "$IMG_EGRESS"; do
		kind load docker-image "$image" --name "$KIND_CLUSTER" >/dev/null
	done
	wait_for "controller deployment ready" 120 \
		kubectl -n "$NS" rollout status deploy/fast-sandbox-controller --timeout=10s
	local crd
	for crd in sandboxpools sandboxtemplates sandboxes sandboxsnapshots; do
		kubectl get crd "$crd.sandbox.fast.io" >/dev/null 2>&1 || die "CRD $crd missing"
	done
	kubectl get crd batchsandboxes.sandbox.opensandbox.io >/dev/null 2>&1 \
		|| die "CRD batchsandboxes.sandbox.opensandbox.io missing"
	pass "CRDs + control plane ready (charts @ pinned $(git -C "$FSB_DIR" rev-parse --short HEAD))"
}

# --- stage: firecracker runtime (node readiness + DART P2P) -----------------------------------

runtime_node_labeled() { # node -> 0 when the agent applied both labels + condition
	local node="$1"
	kubectl get node "$node" -o json 2>/dev/null | jq -e \
		--arg fc "$FC_NODE_LABEL" --arg kvm "$KVM_NODE_LABEL" '
		(.metadata.labels[$fc] == "true") and
		(.metadata.labels[$kvm] == "true") and
		([(.status.conditions // [])[]?
		  | select(.type == "FirecrackerReady" and .status == "True")] | length > 0)
	' >/dev/null
}

runtime_pods() {
	kubectl -n "$NS" get pods -l component=firecracker-runtime -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

dart_roster_ready() { # pod expected-members
	local pod="$1" expected="$2" members
	members="$(kubectl exec -n "$NS" "$pod" -- sh -c \
		'curl -fsS --noproxy "*" http://127.0.0.1:8147/admin/members' 2>/dev/null || true)"
	[[ "$(printf '%s' "$members" | grep -o '"id":' | wc -l | tr -d ' ')" == "$expected" ]]
}

runtime_up() {
	# DaemonSet + dart Service ship with charts/fast-sandbox; credentials land
	# in credentials_up before this stage.
	wait_for "firecracker-runtime DaemonSet ready" 120 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime --timeout=10s

	# Positive wiring assertions: DART admin plane up, agent p2pUp=true
	# (a missing P2P daemon only degrades pulls to direct S3).
	local pod uid node pods
	pods="$(runtime_pods)"
	for pod in $pods; do
		uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart admin /healthz on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' http://127.0.0.1:8147/healthz | grep -q ok"
		wait_for "agent health p2pUp on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' --unix-socket /run/fast-sandbox/firecracker/runtime.sock -H 'Content-Type: application/json' -d '{\"podUid\":\"$uid\",\"namespace\":\"$NS\"}' http://firecracker-agent/v1/health | grep -q '\"p2pUp\":true'"
		log "dart: $node dart pid=$(kubectl exec -n "$NS" "$pod" -- sh -c 'pgrep -x dart')"
	done
	# P2P roster: every daemon must see every peer before any pull, so later
	# nodes are fed by peers instead of the origin.
	local expected_members
	expected_members="$(printf '%s' "$pods" | wc -w | tr -d ' ')"
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart roster full on $node ($expected_members members)" 90 \
			dart_roster_ready "$pod" "$expected_members"
	done
	# The readiness loop applies labels + FirecrackerReady itself.
	for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
		# The agent's host-readiness recheck cadence is 5 minutes; when a
		# first pass fails (e.g. a slow tool install), the labels land on
		# the NEXT pass, so the wait must span one full recheck cycle.
		wait_for "node $node labeled + FirecrackerReady" 480 \
			runtime_node_labeled "$node"
	done
	pass "firecracker-runtime healthy + DART roster=$expected_members + nodes FirecrackerReady"
}

# --- stage (opensandbox CLI): SandboxTemplate golden image ---------------------

wait_succeeded() { # description attempts probe probe_failed
	local description="$1" attempts="$2" probe="$3" probe_failed="$4" attempt=0
	while ! "$probe" >/dev/null 2>&1; do
		if "$probe_failed" >/dev/null 2>&1; then
			failure_dump "template-failed"
			fail "$description (template entered Failed)"
		fi
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			failure_dump "template-timeout"
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

# Template build via the CLI (server /templates API behind it): the row is
# projected onto a SandboxTemplate CRD; the build runs in fast-sandbox, so
# the builder image must be in the cluster.
TEMPLATE_ID=""

_template_phase() {
	osb_api template get "$TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.phase // empty'
}

template_succeeded() {
	[[ "$(_template_phase)" == "Succeeded" ]]
}

template_failed() {
	local phase message
	phase="$(_template_phase)"
	[[ "$phase" == "Failed" ]] || return 1
	message="$(osb_api template get "$TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.message // empty')"
	log "template Failed: ${message:-<no message>}"
	return 0
}

template_up() {
	log "building the sandboxtemplate-builder image"
	if ! docker image inspect "$IMG_OCI_TOOLS" >/dev/null 2>&1; then
		log "building shared oci build base ($IMG_OCI_TOOLS)"
		# shellcheck disable=SC2086
		docker build ${DOCKER_BUILD_FLAGS:-} --quiet -t "$IMG_OCI_TOOLS" \
			-f "$FSB_DIR/build/Dockerfile.oci-tools" "$FSB_DIR" >/dev/null \
			|| die "oci-tools image build failed"
	fi
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		--build-arg OCI_TOOLS_IMAGE="$IMG_OCI_TOOLS" -t "$IMG_BUILDER" \
		-f "$FSB_DIR/build/Dockerfile.sandboxtemplate-builder" "$FSB_DIR" >/dev/null \
		|| die "sandboxtemplate-builder image build failed"
	if [[ "$OCI_STACK" == 1 ]]; then
		bake_registry_ca "$IMG_BUILDER"
		verify_image_ca_bake "$IMG_BUILDER"
		probe_registry_tls
	fi
	kind load docker-image "$IMG_BUILDER" --name "$KIND_CLUSTER" >/dev/null
	local created
	local -a tmpl_args=(
		template create
		--image "$SBX_IMAGE"
		--publish "s3://$RUSTFS_BUCKET/publish"
		--format native
		--resource cpu=1 --resource memory=512Mi --resource disk=2Gi
		--warmup-seconds 15
		--metadata origin=fast-sandbox-env
	)
	log "verify: creating the golden-image template via the opensandbox CLI (image=$SBX_IMAGE)"
	created="$(osb_api "${tmpl_args[@]}" -o json 2>/dev/null)" \
		|| fail "template create failed against $SERVER_URL: $(osb_api "${tmpl_args[@]}" 2>&1 >/dev/null | head -c 400)"
	TEMPLATE_ID="$(printf '%s' "$created" | jq -r '.template_id')"
	printf '%s' "$TEMPLATE_ID" > "$WORK/template-id"
	[[ -n "$TEMPLATE_ID" && "$TEMPLATE_ID" != "null" ]] || fail "template create response carried no templateId"
	log "template id=$TEMPLATE_ID"
	wait_succeeded "template phase=Succeeded" 300 template_succeeded template_failed
	local manifest_ref
	manifest_ref="$(osb_api template get "$TEMPLATE_ID" -o json | jq -r '.status.manifest_ref // empty')"
	[[ -n "$manifest_ref" ]] || fail "template manifestRef is empty"
	log "template manifestRef: $manifest_ref"
	# OSEP-0022 CA wiring: the server filled spec.egressMitmCertificate from
	# template_mitm_ca_secret (first real exercise of the server-side fill),
	# and the builder stamped the baked certificate's fingerprint into the
	# published manifest lineage.
	local cert_ref lineage_fp
	cert_ref="$(kubectl -n "$RESOURCE_NS" get sandboxtemplate "$TEMPLATE_ID" \
		-o jsonpath='{.spec.egressMitmCertificate.secretRef.name}' 2>/dev/null)"
	[[ "$cert_ref" == "$MITM_CA_SECRET" ]] \
		|| fail "template CR carries no egressMitmCertificate secretRef (got '$cert_ref', want $MITM_CA_SECRET)"
	local cert_key
	cert_key="$(kubectl -n "$RESOURCE_NS" get sandboxtemplate "$TEMPLATE_ID" \
		-o jsonpath='{.spec.egressMitmCertificate.key}' 2>/dev/null)"
	[[ "$cert_key" == "ca.crt" ]] \
		|| fail "template CR egressMitmCertificate.key is '$cert_key', want ca.crt (certificate entry only)"
	lineage_fp="$(template_mitm_lineage_fingerprint)"
	[[ "$lineage_fp" == "$MITM_CA_FINGERPRINT" ]] \
		|| fail "template manifest platform.egressMitmCA.certSha256 is '${lineage_fp:-empty}', want pinned $MITM_CA_FINGERPRINT"
	log "template baked CA: secret/$cert_ref (ca.crt), lineage fingerprint ${lineage_fp:0:16}…"
	pass "SandboxTemplate Succeeded + artifacts published (via opensandbox CLI)"
	return 0
}

# An overlaybd template beside the native one: same inputs, published to
# the in-cluster registry. status.manifestRef is the runnable artifact.
OCI_TEMPLATE_ID=""

_overlaybd_template_phase() {
	osb_api template get "$OCI_TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.phase // empty'
}

_overlaybd_template_failed() {
	local phase message
	phase="$(_overlaybd_template_phase)"
	[[ "$phase" == "Failed" ]] || return 1
	message="$(osb_api template get "$OCI_TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.message // empty')"
	log "overlaybd template Failed: ${message:-<no message>}"
	return 0
}

template_up_overlaybd() {
	log "verify: creating the overlaybd template via the opensandbox CLI (publish=oci://$OCI_REGISTRY_HOST/$OCI_REGISTRY_REPO)"
	local created create_err="$WORK/overlaybd-create.err"
	local -a ovl_args=(
		template create
		--image "$SBX_IMAGE"
		--publish "oci://$OCI_REGISTRY_HOST/$OCI_REGISTRY_REPO"
		--format overlaybd
		--resource cpu=1 --resource memory=512Mi --resource disk=2Gi
		--warmup-seconds 15
		--metadata origin=fast-sandbox-env-oci
	)
	created="$(osb_api "${ovl_args[@]}" -o json 2>"$create_err")" \
		|| fail "overlaybd template create failed against $SERVER_URL: $(head -c 400 "$create_err")"
	OCI_TEMPLATE_ID="$(printf '%s' "$created" | jq -r '.template_id')"
	[[ -n "$OCI_TEMPLATE_ID" && "$OCI_TEMPLATE_ID" != "null" ]] || fail "overlaybd template create carried no templateId"
	printf '%s' "$OCI_TEMPLATE_ID" > "$WORK/overlaybd-template-id"
	local attempts=0
	while ! _overlaybd_template_phase_is_succeeded; do
		if _overlaybd_template_failed; then
			failure_dump "overlaybd-template-failed"
			fail "overlaybd template entered Failed"
		fi
		attempts=$((attempts + 1))
		# OCI builds run the convertor + a Firecracker cold boot.
		[[ "$attempts" -ge 450 ]] && { failure_dump "overlaybd-template-timeout"; fail "overlaybd template not Succeeded after 450 attempts"; }
		sleep 2
	done
	local manifest_ref
	manifest_ref="$(osb_api template get "$OCI_TEMPLATE_ID" -o json | jq -r '.status.manifest_ref // empty')"
	[[ "$manifest_ref" == oci://* ]] || fail "overlaybd template manifestRef is not an oci:// reference: $manifest_ref"
	log "overlaybd template manifestRef: $manifest_ref"
	pass "overlaybd SandboxTemplate Succeeded (OCI publication)"
}

_overlaybd_template_phase_is_succeeded() {
	[[ "$(_overlaybd_template_phase)" == "Succeeded" ]]
}

# --- stage: OverlayBD OCI registry (CA, image CA-bake, in-cluster registry) ---

# bake_registry_ca retags a built image with the registry CA trusted; the
# upstream Dockerfiles take no CA input. Consumers: fastlet (fsb-vm), the
# firecracker-runtime (ublkd), the sandboxtemplate-builder (push).
bake_registry_ca() { # <image>
	local image="$1"
	docker image inspect "$image" >/dev/null 2>&1 || die "image $image missing for the registry CA bake"
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		--build-arg BASE_IMAGE="$image" \
		-t "$image" -f "$CA_DIR/Dockerfile.ca-bake" "$CA_DIR" >/dev/null \
		|| die "registry CA bake failed for $image"
}

registry_ca_up() {
	[[ "$OCI_STACK" == 1 ]] || {
		log "OCI stack disabled (OCI_STACK=0); skipping the registry CA"
		return 0
	}
	command -v openssl >/dev/null 2>&1 || die "openssl is required for the OCI registry certificates"
	mkdir -p "$CA_DIR"
	# The CA persists in $WORK; the serving cert is re-issued per run.
	# The CA needs a subjectKeyIdentifier (1.1.1 `req -x509` adds none) and
	# no duplicate extensions (host v3_ca section + -addext stacks them);
	# either defect makes OpenSSL 3 clients fail chain validation
	# (cache_extensions reason(158)). Regenerate on either.
	if [[ ! -s "$CA_DIR/ca.crt" || ! -s "$CA_DIR/ca.key" ]] \
		|| ! openssl x509 -in "$CA_DIR/ca.crt" -noout -text 2>/dev/null | grep -q "Subject Key Identifier" \
		|| [[ "$(openssl x509 -in "$CA_DIR/ca.crt" -noout -text 2>/dev/null | grep -c 'X509v3 Basic Constraints')" -gt 1 ]]; then
		if [[ -s "$CA_DIR/ca.crt" ]]; then
			log "existing CA is missing a subjectKeyIdentifier or carries duplicate extensions; regenerating"
		fi
	# Fully explicit config: the host openssl.cnf (Al8) defaults `req -x509`
	# to its v3_ca section, and combining it with -addext duplicates
	# extensions (Basic Constraints twice) — which OpenSSL 3 clients reject
	# at ossl_x509v3_cache_extensions:reason(158), failing every TLS verify
	# against the registry. A minimal local config keeps the CA deterministic
	# across hosts.
	cat > "$CA_DIR/ca.cnf" <<'CA_CNF'
[req]
distinguished_name = dn
prompt = no

[dn]
CN = fast-sandbox-env-registry-ca

[x509_ext]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
CA_CNF
	openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
		-config "$CA_DIR/ca.cnf" -extensions x509_ext \
		-keyout "$CA_DIR/ca.key" -out "$CA_DIR/ca.crt" >/dev/null 2>&1 \
		|| die "registry CA generation failed"
	fi
	openssl req -newkey rsa:4096 -sha256 -nodes \
		-keyout "$CA_DIR/server.key" -out "$CA_DIR/server.csr" \
		-subj "/CN=$OCI_REGISTRY_HOST" >/dev/null 2>&1 \
		|| die "registry server key generation failed"
	cat > "$CA_DIR/server.ext" <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
subjectAltName=DNS:registry,DNS:registry.$RESOURCE_NS,DNS:registry.$RESOURCE_NS.svc,DNS:$OCI_REGISTRY_HOST
EXT
	openssl x509 -req -sha256 -days 365 \
		-in "$CA_DIR/server.csr" -CA "$CA_DIR/ca.crt" -CAkey "$CA_DIR/ca.key" \
		-CAcreateserial -out "$CA_DIR/server.crt" \
		-extfile "$CA_DIR/server.ext" >/dev/null 2>&1 \
		|| die "registry server certificate signing failed"
	cat > "$CA_DIR/Dockerfile.ca-bake" <<'EOF_DOCKERFILE'
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY ca.crt /usr/local/share/ca-certificates/fast-sandbox-env-registry.crt
RUN update-ca-certificates
EOF_DOCKERFILE
	# The images are retagged in place; build_images/template_up bake right
	# after producing them.
	pass "registry CA ready ($OCI_REGISTRY_HOST)"
}

registry_up() {
	[[ "$OCI_STACK" == 1 ]] || {
		log "OCI stack disabled (OCI_STACK=0); skipping the OCI registry"
		return 0
	}
	docker pull -q "$OCI_REGISTRY_IMAGE" >/dev/null || die "pull $OCI_REGISTRY_IMAGE failed"
	docker pull -q "$OCI_HTPASSWD_IMAGE" >/dev/null || die "pull $OCI_HTPASSWD_IMAGE failed"
	kind load docker-image "$OCI_REGISTRY_IMAGE" --name "$KIND_CLUSTER" >/dev/null
	# Namespace is also ensured by credentials_up; keep this stage self-contained.
	kubectl create namespace "$RESOURCE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# One push+pull account: the node ublkd credential context is service-wide.
	docker run --rm --entrypoint htpasswd "$OCI_HTPASSWD_IMAGE" \
		-Bbn "$OCI_REGISTRY_USER" "$OCI_REGISTRY_PASSWORD" > "$WORK/registry-htpasswd" \
		|| die "htpasswd generation failed"
	kubectl -n "$RESOURCE_NS" create secret generic registry-htpasswd \
		--from-file=htpasswd="$WORK/registry-htpasswd" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$RESOURCE_NS" create secret tls registry-tls \
		--cert="$CA_DIR/server.crt" --key="$CA_DIR/server.key" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	cat > "$GEN_DIR/oci-registry.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry
  namespace: $RESOURCE_NS
  labels: {app: fast-sandbox-env-registry}
spec:
  replicas: 1
  selector:
    matchLabels: {app: fast-sandbox-env-registry}
  template:
    metadata:
      labels: {app: fast-sandbox-env-registry}
    spec:
      containers:
      - name: registry
        image: $OCI_REGISTRY_IMAGE
        ports:
        - containerPort: 5000
        env:
        - name: REGISTRY_HTTP_ADDR
          value: :5000
        - name: REGISTRY_HTTP_TLS_CERTIFICATE
          value: /tls/tls.crt
        - name: REGISTRY_HTTP_TLS_KEY
          value: /tls/tls.key
        - name: REGISTRY_AUTH
          value: htpasswd
        - name: REGISTRY_AUTH_HTPASSWD_REALM
          value: fast-sandbox-env-registry
        - name: REGISTRY_AUTH_HTPASSWD_PATH
          value: /auth/htpasswd
        readinessProbe:
          # /v2/ answers 401 unauthenticated; TLS + auth reachable is the point.
          exec:
            command: [/bin/sh, -ec, 'nc -z 127.0.0.1 5000']
          periodSeconds: 5
        volumeMounts:
        - name: tls
          mountPath: /tls
          readOnly: true
        - name: auth
          mountPath: /auth
          readOnly: true
      volumes:
      - name: tls
        secret: {secretName: registry-tls}
      - name: auth
        secret: {secretName: registry-htpasswd}
---
apiVersion: v1
kind: Service
metadata:
  name: registry
  namespace: $RESOURCE_NS
  labels: {app: fast-sandbox-env-registry}
spec:
  ports:
  - name: https
    port: 443
    targetPort: 5000
  selector:
    app: fast-sandbox-env-registry
YAML
	kubectl apply -f "$GEN_DIR/oci-registry.yaml" >/dev/null
	kubectl -n "$RESOURCE_NS" rollout status deploy/registry --timeout=180s >/dev/null \
		|| { failure_dump "registry-rollout"; fail "the in-cluster OCI registry did not become ready"; }
	verify_registry_trust
	pass "in-cluster OCI registry up (https://$OCI_REGISTRY_HOST)"
}

# probe_registry_tls observes the registry from inside a pod running the
# builder image: the served certificate and whether the system bundle
# validates it.
probe_registry_tls() {
	local out
	out="$(kubectl run registry-tls-probe -n "$RESOURCE_NS" \
		--image="$(docker image inspect --format '{{.ID}}' "$IMG_BUILDER")" \
		--restart=Never --rm -i -q \
		--overrides="{\"spec\":{\"containers\":[{\"name\":\"probe\",\"image\":\"$(docker image inspect --format '{{.ID}}' "$IMG_BUILDER")\",\"command\":[\"/bin/sh\",\"-ec\",\"openssl s_client -connect $OCI_REGISTRY_HOST:443 -servername $OCI_REGISTRY_HOST </dev/null 2>/dev/null > /tmp/served.pem; openssl x509 -in /tmp/served.pem -noout -issuer -subject -dates -fingerprint -sha256; echo verify: \$(openssl verify -CAfile /usr/local/share/ca-certificates/fast-sandbox-env-registry.crt /tmp/served.pem 2>&1); echo curl: \$(curl -sS -o /dev/null -w '%{http_code}' https://$OCI_REGISTRY_HOST/v2/ -u $OCI_REGISTRY_USER:$OCI_REGISTRY_PASSWORD 2>&1 | tail -c 300); wc -c < /etc/ssl/certs/ca-certificates.crt\"],\"securityContext\":{\"privileged\":true}}],\"restartPolicy\":\"Never\"}}" 2>&1 | tail -10)" \
		|| log "registry TLS probe pod failed: $out"
	log "registry TLS probe (in-pod view):"
	printf '%s\n' "$out" | sed 's/^/  | /'
}

# cert_fingerprint prints the sha256 fingerprint of a PEM file.
cert_fingerprint() { # <pem>
	openssl x509 -in "$1" -noout -fingerprint -sha256 | cut -d= -f2
}

# verify_registry_trust checks the serving Secret carries exactly the
# certificate signed this run (leaf-to-leaf fingerprints).
verify_registry_trust() {
	local signed_fp served_fp
	signed_fp="$(cert_fingerprint "$CA_DIR/server.crt")"
	served_fp="$(kubectl -n "$RESOURCE_NS" get secret registry-tls \
		-o jsonpath='{.data.tls\.crt}' | base64 -d | cert_fingerprint /dev/stdin)"
	[[ -n "$served_fp" ]] || { failure_dump "registry-trust"; fail "could not read the registry serving certificate"; }
	[[ "$signed_fp" == "$served_fp" ]] \
		|| { failure_dump "registry-trust"; fail "registry serving cert mismatch: signed=$signed_fp served=$served_fp"; }
	log "signed server cert: $(openssl x509 -in "$CA_DIR/server.crt" -noout -fingerprint -sha256 -dates | tr '\n' ' ')"
	pass "registry serving cert matches the env-signed certificate ($signed_fp)"
}

# verify_image_ca_bake checks the CA landed in the image and was merged
# into its system bundle.
verify_image_ca_bake() { # <image>
	local ca_fp body
	ca_fp="$(cert_fingerprint "$CA_DIR/ca.crt")"
	local in_img
	in_img="$(docker run --rm --entrypoint sh "$1" -c '
		openssl x509 -in /usr/local/share/ca-certificates/fast-sandbox-env-registry.crt -noout -fingerprint -sha256
	' 2>/dev/null | cut -d= -f2)"
	[[ "$in_img" == "$ca_fp" ]] \
		|| { failure_dump "ca-bake"; fail "CA bake mismatch for $1: CA=$ca_fp image=$in_img"; }
	body="$(grep -v -- '---' "$CA_DIR/ca.crt" | tr -d '\n')"
	# The merged bundle keeps the PEM folded at 64 columns, so compare
	# against the flattened bundle, not the raw file.
	docker run --rm --entrypoint sh "$1" -c "
		tr -d '\n' < /etc/ssl/certs/ca-certificates.crt | grep -c '$body'
	" >/dev/null \
		|| { failure_dump "ca-bake"; fail "CA not merged into the system bundle of $1 (update-ca-certificates)"; }
	pass "CA baked + merged in $1 ($ca_fp)"
}

bake_registry_ca_into_images() {
	[[ "$OCI_STACK" == 1 ]] || return 0
	log "baking the registry CA into the fsb images"
	bake_registry_ca "$IMG_FASTLET"
	bake_registry_ca "$IMG_RUNTIME"
	verify_image_ca_bake "$IMG_FASTLET"
	verify_image_ca_bake "$IMG_RUNTIME"
}

# template_mitm_lineage_fingerprint [template-id] -> the certSha256 the
# builder stamped under platform.egressMitmCA in the published artifact-set
# manifest (empty when the manifest or the entry is missing).
template_mitm_lineage_fingerprint() {
	local tid manifest_ref manifest_json
	tid="${1:-$TEMPLATE_ID}"
	manifest_ref="$(osb_api template get "$tid" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')"
	[[ -n "$manifest_ref" ]] || return 0
	manifest_json="$(rc cat "chain/$RUSTFS_BUCKET/$(dirname "${manifest_ref#s3://$RUSTFS_BUCKET/}")/manifest.json" 2>/dev/null)" || return 0
	printf '%s' "$manifest_json" | jq -r '.platform.egressMitmCA.certificates[0].certSha256 // empty' 2>/dev/null
}

# --- stage: SandboxPool (egress attached, P2P spread) --------------------------------------------

pool_pods() {
	kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

first_pool_pod() {
	kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

fastlet_pods_ready() {
	local ready=0 pod
	for pod in $(pool_pods); do
		if kubectl -n "$RESOURCE_NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
			ready=$((ready + 1))
		fi
	done
	[[ "$ready" -ge "$POOL_MIN" ]]
}

egress_containers_ready() {
	local pods pod
	pods="$(pool_pods)"
	[[ -n "$pods" ]] || return 1
	for pod in $pods; do
		kubectl -n "$RESOURCE_NS" get pod "$pod" -o jsonpath='{range .status.containerStatuses[*]}{.name}{"="}{.ready}{" "}{end}' 2>/dev/null \
			| grep -q 'egress=true' || return 1
	done
}

# GET /_fastlet/v1/actions/status must echo the shared apiVersion, ready=true
# and a non-empty instanceId; the handler binds Pod loopback only, so the
# probe runs inside the egress container.
egress_status_ready() {
	local pod out
	pod="$(first_pool_pod)"
	[[ -n "$pod" ]] || return 1
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$pod" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/_fastlet/v1/actions/status" 2>/dev/null)" || return 1
	[[ "$out" == *'"apiVersion":"sandbox.fast.io/actions/v1"'* && "$out" == *'"ready":true'* && "$out" == *'"instanceId":'* ]]
}

pool_status_ready() {
	local ready
	ready="$(kubectl_get "sandboxpool/$POOL_NAME" '{.status.readyPods}' 2>/dev/null || true)"
	[[ "$ready" =~ ^[0-9]+$ ]] && (( ready >= POOL_MIN ))
}

pool_condition_true() { # condition-type
	[[ "$(kubectl_get "sandboxpool/$POOL_NAME" "{.status.conditions[?(@.type==\"$1\")].status}" 2>/dev/null)" == "True" ]]
}

warm_images_ready() {
	kubectl -n "$RESOURCE_NS" get sandboxpool "$POOL_NAME" -o jsonpath='{.status.warmImages[*].cachedFastlets}' 2>/dev/null | grep -qv '^0*$'
}

# --- stage: ublk on the host and inside the kind nodes (OCI_STACK=1) ------------

# OverlayBD talks to the HOST kernel's ublk module through /dev/ublkc* (the
# control char device) and surfaces devices as /dev/ublkb<N>. The kernel is
# shared with the kind nodes, but the device NODES are not: a kind node's
# /dev is a private tmpfs populated at node creation, while /dev/ublkc*
# appears on the host only at modprobe time — usually after the cluster
# exists. Every ublkd/fsb-vm consumer (the builder Pod, the node ublkd
# sidecar, fastlets) mounts the kind node's /dev, so the nodes need the nodes.
# --- stage: ublk registration check (OCI_STACK=1) -------------------------------

# Informational registration check only. Naming varies by kernel: the char
# (queue) device class is "ublk-char" (Al8 6.6 backport, /dev/ublkc<N>) or
# "ublkc" (mainline), the control node is /dev/ublk-control (misc) or
# /dev/ublkc, and some kernels register NO block major — ublkb nodes use
# per-disk dynamic majors that ublkd mknods itself. The runner may be a
# container whose /dev hides these nodes; /proc/devices always reflects the
# shared kernel.
ublk_module_up() {
	[[ "$OCI_STACK" == 1 ]] || {
		log "OCI_STACK=0: skipping the ublk check (overlaybd disabled)"
		return 0
	}
	# The overlaybd stack is unusable without the kernel module: the node
	# ublkd sidecar crash-loops without /dev/ublk-control and every build
	# fails at its probe device, so fail here instead of ~16 minutes later.
	if ! grep -qiE 'ublk-char|ublkc' /proc/devices; then
		log "ublk not registered; attempting modprobe ublk_drv"
		sudo_ modprobe ublk_drv 2>/dev/null || true
	fi
	grep -qiE 'ublk-char|ublkc' /proc/devices \
		|| die "ublk is not registered in /proc/devices after modprobe: the kernel needs CONFIG_BLK_DEV_UBLK (and the runner must be able to load modules); overlaybd requires it when OCI_STACK=1"
	log "ublk registered: $(grep -i ublk /proc/devices | tr '\n' ' ')"
	pass "ublk module registered"
}

render_pool() { # > $GEN_DIR/firecracker-egress-pool.yaml
	local src="$MANIFESTS_DIR/pool/firecracker-egress-pool.yaml" out="$GEN_DIR/firecracker-egress-pool.yaml"
	mkdir -p "$GEN_DIR"
	# Quoted tokens in the template keep rendered scalars' natural types
	# (poolMin stays an integer); awk for sed portability.
	awk -v fastlet="$IMG_FASTLET" -v egress="$IMG_EGRESS" \
		-v pool_min="$POOL_MIN" -v pool_max="$POOL_MAX" \
		-v max_per_pod="$MAX_SANDBOXES_PER_POD" \
		-v warm="$WARM_IMAGES" -v image="$TEMPLATE_ID" \
		-v mitm_ca="$MITM_CA_SECRET" '
		{ gsub(/"@FASTLET_IMAGE@"/, fastlet)
		  gsub(/"@EGRESS_IMAGE@"/, egress)
		  gsub(/"@POOL_MIN@"/, pool_min)
		  gsub(/"@POOL_MAX@"/, pool_max)
		  gsub(/"@MITM_CA_SECRET@"/, mitm_ca)
		  gsub(/maxSandboxesPerPod: 5/, "maxSandboxesPerPod: " max_per_pod) }
		/^# @WARM_IMAGES@$/ {
			if (warm == "1") printf "  warmImages:\n  - %s\n", image
			next }
		{ print }
	' "$src" > "$out"
	# OverlayBD fragment: the OCI flag appends to the fastlet's env list and
	# the shared node mounts (tools read-only, the ublkd control socket, the
	# host /dev so ublk devices reach the VMs) append to its volumeMounts —
	# two separate tokens so neither YAML list gains a duplicate key. The
	# registry credentials ride the pool controller's compiled
	# fast-sandbox-registry projection (env + volume injected by the
	# controller), not this manifest.
	if [[ "$OCI_STACK" == 1 ]]; then
		local oci_env oci_mounts volumes
		oci_env="$(cat <<'ENVS'
        - name: FAST_SANDBOX_OCI_ENABLED
          value: "true"
ENVS
)"
		oci_mounts="$(cat <<'MOUNTS'
        - name: oci-tools
          mountPath: /opt/fast-sandbox/oci
          readOnly: true
        - name: oci-socket
          mountPath: /run/fast-sandbox/overlaybd
        - name: host-dev
          mountPath: /dev
MOUNTS
)"
		volumes="$(cat <<'VOLUMES'
      - name: oci-tools
        hostPath:
          path: /opt/fast-sandbox/oci
          type: DirectoryOrCreate
      - name: oci-socket
        hostPath:
          path: /run/fast-sandbox/overlaybd
          type: DirectoryOrCreate
      - name: host-dev
        hostPath:
          path: /dev
          type: Directory
VOLUMES
)"
		# POSIX-safe token substitution: BSD awk rejects newline-bearing -v
		# values, so the fragments travel through ENVIRON instead.
		oci_env="$oci_env" oci_mounts="$oci_mounts" oci_volumes="$volumes" awk '
			/^# @OCI_FASTLET_ENV@$/ { printf "%s\n", ENVIRON["oci_env"]; next }
			/^# @OCI_FASTLET_MOUNTS@$/ { printf "%s\n", ENVIRON["oci_mounts"]; next }
			/^# @OCI_FASTLET_VOLUMES@$/ { printf "%s\n", ENVIRON["oci_volumes"]; next }
			{ print }
		' "$out" > "$out.tmp" && mv "$out.tmp" "$out"
	else
		grep -v '^# @OCI_FASTLET_' "$out" > "$out.tmp" && mv "$out.tmp" "$out"
	fi
	# Leftover-token guard: match only value positions (key: ...@T@...),
	# never the header comments that document the tokens themselves.
	if grep -Eq '^[[:space:]]*[A-Za-z][A-Za-z0-9]*:.*@[A-Z_]+@' "$out"; then
		die "unrendered token left in $out"
	fi
}

pool_up() {
	load_mitm_ca_fingerprint
	[[ -n "$MITM_CA_FINGERPRINT" ]] || die "no pinned MITM CA fingerprint at $MITM_CA_FINGERPRINT_FILE (run up first; mitm_ca_up stores it there)"
	render_pool
	log "applying SandboxPool $POOL_NAME (runtime=firecracker, egress attached, MITM CA pinned, poolMin=$POOL_MIN)"
	kubectl apply -f "$GEN_DIR/firecracker-egress-pool.yaml" >/dev/null
	wait_for "fastlet pods Ready (poolMin=$POOL_MIN)" 300 fastlet_pods_ready
	wait_for "egress container ready in every fastlet pod" 180 egress_containers_ready
	wait_for "pool condition RuntimeReady=True" 120 pool_condition_true RuntimeReady
	wait_for "pool condition InfraReady=True" 120 pool_condition_true InfraReady
	wait_for "egress actions endpoint answering (/_fastlet/v1/actions/status)" 60 egress_status_ready
	wait_for "egress MITM addon up with the pinned CA fingerprint" 120 egress_mitm_ca_pinned
	if [[ "$WARM_IMAGES" == "1" ]]; then
		wait_for "pool warmImages Ready" 300 warm_images_ready
		p2p_evidence "warm preheat"
		pass "fastlet Running + warmImages Ready (P2P evidence captured)"
	else
		pass "fastlet Running, on-demand pull (default: first sandbox pulls through DART)"
	fi
}

# egress_mitm_ca_fingerprint -> the pinned CA fingerprint the egress exposes
# on /healthz (empty while the MITM addon is down/pending or not yet up).
egress_mitm_ca_fingerprint() {
	local pod out
	pod="$(first_pool_pod)"
	[[ -n "$pod" ]] || return 1
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$pod" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/healthz" 2>/dev/null)" || return 1
	printf '%s' "$out" | sed -n 's/.*"mitmCaFingerprint":"\([0-9a-f]*\)".*/\1/p'
}

# Content pinning (OSEP-0022): the egress seed's fingerprint must equal the
# pinned CA's — a same-named Secret copy carrying different (but valid)
# material fails here loudly instead of degrading into opaque guest TLS
# failures.
egress_mitm_ca_pinned() {
	local fp
	fp="$(egress_mitm_ca_fingerprint)"
	[[ -n "$fp" && "$fp" == "$MITM_CA_FINGERPRINT" ]]
}

# Assert the P2P outcome from DART block counters: the artifact set is
# pulled ~once per 4MiB block from the origin cluster-wide, and extra nodes
# must be fed by peers, not the origin.
p2p_evidence() { # description
	local description="$1"
	local pods pod manifest_ref manifest_key build_dir expected_blocks=0 stat_json
	local origin_total=0 peer_total=0 cache_total=0 size source value active_nodes=0 node_total
	manifest_ref="$(osb_api template get "$TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')"
	manifest_key="${manifest_ref#s3://$RUSTFS_BUCKET/}"
	build_dir="$(dirname "$manifest_key")"
	local object
	for object in rootfs.ext4 vmstate.snap memory.snap; do
		stat_json="$(rc stat --json "chain/$RUSTFS_BUCKET/$build_dir/$object" 2>&1)" \
			|| die "rc stat failed for published $object (publish incomplete?): $stat_json"
		size="$(jq -er '.size_bytes | numbers' <<<"$stat_json" 2>/dev/null)" \
			|| die "rc stat returned no numeric size_bytes for published $object"
		expected_blocks=$((expected_blocks + (size + 4194303) / 4194304))
	done
	pods="$(runtime_pods)"
	for pod in $pods; do
		node_total=0
		while read -r source value; do
			case "$source" in
				origin) origin_total=$((origin_total + value)); node_total=$((node_total + value)) ;;
				peer) peer_total=$((peer_total + value)); node_total=$((node_total + value)) ;;
				cache) cache_total=$((cache_total + value)); node_total=$((node_total + value)) ;;
			esac
		done < <(dart_source_counters "$pod")
		[[ "$node_total" -gt 0 ]] && active_nodes=$((active_nodes + 1))
	done
	log "p2p evidence ($description): expected origin=$expected_blocks blocks; cluster origin=$origin_total peer=$peer_total cache=$cache_total active-nodes=$active_nodes"
	[[ "$origin_total" -ge "$expected_blocks" ]] || fail "cluster origin $origin_total < expected $expected_blocks blocks"
	[[ "$origin_total" -le $((expected_blocks + 4)) ]] \
		|| fail "origin amplified: $origin_total > $((expected_blocks + 4)): pulls were not deduplicated by DART"
	if [[ "$active_nodes" -ge 2 ]]; then
		[[ "$peer_total" -gt 0 ]] || fail "no peer traffic across $active_nodes nodes: the second node was not served by the peer"
		pass "P2P evidence ($description): origin ~1 fetch per block (cluster=$origin_total/$expected_blocks), peer=$peer_total, nodes=$active_nodes"
	else
		pass "P2P evidence ($description): origin ~1 fetch per block (cluster=$origin_total/$expected_blocks) on $active_nodes node (no peer needed)"
	fi
}

dart_source_counters() { # pod -> "<source> <value>" lines
	local pod="$1"
	kubectl exec -n "$NS" "$pod" -- sh -c 'curl -fsS --noproxy "*" http://127.0.0.1:8147/metrics' 2>/dev/null \
		| awk '/^dart_block_source_total\{source="(cache|peer|origin)"\}/ {
			match($0, /source="[^"]+"/); s = substr($0, RSTART + 8, RLENGTH - 9)
			match($0, /} [0-9]+$/); print s, substr($0, RSTART + 2)
		}'
}

# --- stage: idle-pause Redis (OSEP-0024 profile) --------------------------------

redis_up() {
	# The sweeper connects at startup, so Redis must precede the server.
	[[ "$IDLE_PAUSE" == "1" ]] || { log "idle-pause profile off (IDLE_PAUSE=0); skipping Redis"; rm -f "$WORK/idle-pause"; return 0; }
	touch "$WORK/idle-pause"
	mkdir -p "$GEN_DIR"
	sed "s|image: redis:7-alpine|image: $REDIS_IMAGE|" \
		"$MANIFESTS_DIR/idle-pause/redis.yaml" > "$GEN_DIR/idle-pause-redis.yaml"
	kubectl apply -n "$OSB_NS" -f "$GEN_DIR/idle-pause-redis.yaml" >/dev/null
	wait_for "idle-pause Redis deployment ready" 120 \
		kubectl -n "$OSB_NS" rollout status deploy/idle-pause-redis --timeout=10s
	pass "idle-pause Redis up ($REDIS_DSN)"
}

# --- stage: OpenSandbox server + ingress gateway --------------------------------

# Per-workdir key (not per-run): re-applies must keep old routes verifiable.
osb_signing_key() {
	if [[ ! -s "$SIGNING_KEY_FILE" ]]; then
		openssl rand -base64 32 | tr -d '\n' > "$SIGNING_KEY_FILE"
	fi
	cat "$SIGNING_KEY_FILE"
}

# Server config.toml (fsb runtime) with workdir tokens; the gateway [ingress]
# block is appended by charts/server from server.gateway.* values.
render_server_config() { # > $GEN_DIR/osb-server-config.toml
	mkdir -p "$GEN_DIR"
	awk -v api_key="$SERVER_API_KEY" \
		-v fastpath="$FASTPATH_ENDPOINT" -v fsb_ns="$RESOURCE_NS" -v pool="$POOL_NAME" \
		-v execd="$EXECD" -v oci_secret="$OCI_REGISTRY_SECRET" -v oci_stack="$OCI_STACK" \
		-v mitm_ca="$MITM_CA_SECRET" '
		{ if ($0 == "# @OCI_PUBLISH_SECRET@") {
		    # emitted in-place so the key stays inside [kubernetes].
		    if (oci_stack == "1")
		      printf "template_oci_publish_secret = \"%s\"\n", oci_secret
		    next }
		  gsub(/@SERVER_API_KEY@/, api_key)
		  gsub(/@SIGNING_KEY@/, signing_key)
		  gsub(/@FASTPATH_ENDPOINT@/, fastpath)
		  gsub(/@FSB_NAMESPACE@/, fsb_ns)
		  gsub(/@POOL_NAME@/, pool)
		  gsub(/@EXECD_IMAGE@/, execd)
		  gsub(/@MITM_CA_SECRET@/, mitm_ca)
		  gsub(/@GATEWAY_ADDRESS@/, gateway)
		  print }
	' <<'TOML' > "$GEN_DIR/osb-server-config.toml"
[server]
host = "0.0.0.0"
port = 80
api_key = "@SERVER_API_KEY@"

[log]
level = "INFO"

[runtime]
type = "kubernetes"
execd_image = "@EXECD_IMAGE@"

[kubernetes]
# One block for CR reads + fsb settings; sandboxes live in the pool's
# namespace (poolRef resolves) and get the execd image injected by the server.
namespace = "@FSB_NAMESPACE@"
fastpath_endpoint = "@FASTPATH_ENDPOINT@"
fastpath_resource_pool = "@POOL_NAME@"
fastpath_wait_ready_seconds = 30.0
template_s3_publish_secret = "sandbox-oss-credentials"
# @OCI_PUBLISH_SECRET@
# OSEP-0022 MITM trust anchor: server-created SandboxTemplates bake
# spec.egressMitmCertificate from the pinned CA Secret (certificate entry
# only, never the private key), so the builder installs it into the guest
# trust store at build time.
template_mitm_ca_secret = "@MITM_CA_SECRET@"
template_mitm_ca_secret_key = "ca.crt"
TOML
	if [[ "$IDLE_PAUSE" == "1" ]]; then
		# Dotted keys nest the redis sub-table.
		cat >> "$GEN_DIR/osb-server-config.toml" <<TOML

[automation.idle_pause]
enabled = true
sweep_interval_seconds = 5
activity_ttl_seconds = 1800
redis.dsn = "$REDIS_DSN"
redis.lock_ttl_seconds = 30
TOML
	fi
	if grep -Eq '@[A-Z_]+@' "$GEN_DIR/osb-server-config.toml"; then
		die "unrendered token left in $GEN_DIR/osb-server-config.toml"
	fi
}

opensandbox_up() {
	kind load docker-image "$IMG_SERVER" --name "$KIND_CLUSTER" >/dev/null
	kind load docker-image "$IMG_INGRESS" --name "$KIND_CLUSTER" >/dev/null
	render_server_config
	apply_ns "$OSB_NS"
	# charts/server has the fsb RBAC built in; the config carries the wiring.
	helm_render opensandbox-server "$OSB_ROOT/manifests/charts/server" "$OSB_NS" \
		"$GEN_DIR/osb-server.yaml" \
		--set server.image.repository="$(image_repo "$IMG_SERVER")" \
		--set server.image.tag="$(image_tag "$IMG_SERVER")" \
		--set server.service.type=NodePort \
		--set server.service.nodePort="$SERVER_NODEPORT" \
		--set server.resources.requests.cpu=250m \
		--set server.resources.requests.memory=512Mi \
		--set server.resources.limits.cpu=1 \
		--set server.resources.limits.memory=2Gi \
		--set-file configToml="$GEN_DIR/osb-server-config.toml" \
		--set server.gateway.enabled=true \
		--set server.gateway.host="$GATEWAY_ADDRESS" \
		--set server.gateway.gatewayRouteMode=header \
		--set server.gateway.secureAccess.activeKey=a \
		--set "server.gateway.secureAccess.keys[0].key_id=a" \
		--set "server.gateway.secureAccess.keys[0].key=$(osb_signing_key)"
		kubectl apply -f "$GEN_DIR/osb-server.yaml" >/dev/null
	# fast-sandbox provider via FastPath, same signing key as the server.
	local gateway_idle_args=()
	if [[ "$IDLE_PAUSE" == "1" ]]; then
		gateway_idle_args=(
			--set gateway.activity.enabled=true
			--set gateway.activity.redisDsn="$REDIS_DSN"
			--set gateway.activity.minInterval=5s
			--set gateway.activity.ttlSeconds=1800
			--set gateway.wake.enabled=true
			--set gateway.wake.parkBudget="${IDLE_PAUSE_PARK_BUDGET:-5s}"
		)
	fi
	helm_render opensandbox-ingress-gateway "$OSB_ROOT/manifests/charts/ingress-gateway" "$OSB_NS" \
		"$GEN_DIR/osb-ingress-gateway.yaml" \
		--set gateway.image.repository="$(image_repo "$IMG_INGRESS")" \
		--set gateway.image.tag="$(image_tag "$IMG_INGRESS")" \
		--set gateway.replicaCount=1 \
		--set gateway.providerType=fast-sandbox \
		--set gateway.dataplaneNamespace="$NS" \
		--set gateway.fastpathEndpoint="$FASTPATH_ENDPOINT" \
		--set "gateway.secureAccess.keys[0].key_id=a" \
		--set "gateway.secureAccess.keys[0].key=$(osb_signing_key)" \
		--set gateway.service.type=NodePort \
		--set gateway.service.nodePort="$GATEWAY_NODEPORT" \
		--set gateway.resources.requests.cpu=100m \
		--set gateway.resources.requests.memory=128Mi \
		--set gateway.resources.limits.cpu=1 \
		--set gateway.resources.limits.memory=1Gi \
		"${gateway_idle_args[@]}"
	kubectl apply -f "$GEN_DIR/osb-ingress-gateway.yaml" >/dev/null
	wait_for "server deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-server --timeout=10s
	wait_for "ingress gateway deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-ingress-gateway --timeout=10s
	# A ready gateway proves FastPath reachability: it fails startup without
	# the gRPC connection.
	wait_for "server /health on 127.0.0.1:$SERVER_HOST_PORT" 60 \
		curl -fsS -m 5 "$SERVER_URL/health"
	wait_for "gateway /status.ok on $GATEWAY_ADDRESS" 60 \
		curl -fsS -m 5 --noproxy "*" "$GATEWAY_URL/status.ok"
	pass "server + ingress gateway up (fsb runtime, gateway routes signed with key 'a')"
}

# Raw curl, kept for the calls the CLI cannot express (networkpolicy
# replace PUT, metadata merge-patch, snapshot re-entry fence).
server_api() { # method path [json-body]
	local method="$1" path="$2" body="${3:-}"
	if [[ -n "$body" ]]; then
		curl -fsS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-H "Content-Type: application/json" -d "$body" "$SERVER_URL$path"
	else
		curl -fsS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" "$SERVER_URL$path"
	fi
}

# opensandbox CLI pointed at the lifecycle server; --no-color keeps
# -o json parseable.
osb_api() {
	"$OSB_BIN" --no-color --domain "127.0.0.1:$SERVER_HOST_PORT" --protocol http \
		--api-key "$SERVER_API_KEY" --request-timeout 60 "$@"
}

# The server assigns the sandbox id (CreateSandboxRequest carries none);
# the verify probes share it through VERIFY_ID.
VERIFY_ID=""
# The snapshot verify stage shares the created snapshot ids through
# SNAPSHOT_ID / SNAPSHOT_ID2.
SNAPSHOT_ID=""
SNAPSHOT_ID2=""
# A re-entry snapshot accepted during fence cache lag (202) is tracked here
# so its terminal outcome is asserted and it is cleaned up.
SNAPSHOT_EXTRA=""

verify_sandbox_gone() {
	! osb_api sandbox get "$VERIFY_ID" -o json >/dev/null 2>&1
}

verify_policy_enforced() {
	local out
	out="$(server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.mode // empty')" == "enforcing" ]] || return 1
	[[ "$(printf '%s' "$out" | jq -r '.policy.egress[0].target // empty')" == "example.com" ]]
}

verify_policy_updated() {
	local out
	out="$(server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.mode // empty')" == "enforcing" ]] || return 1
	[[ "$(printf '%s' "$out" | jq -r '.policy.egress[0].target // empty')" == "github.com" ]]
}

# Full wire-up: raw-API create (fsb) -> FastPath -> fastlet -> firecracker
# sandbox -> signed gateway route -> fastlet-proxy -> guest execd /ping ->
# delete. One sandbox per call; availability is measured from the client
# (poll /ping until 200, wall-clock budget — every poll is curl-only, so
# readings resolve ~10ms), not from CR status. The timed windows wrap HTTP
# round trips only: osb invocations would bill ~0.5s of interpreter startup
# to each reading, which is exactly the measurement pollution #2323 called
# out. CLI create coverage lives in the pause-resume/vault/snapshot verifies
# and the lifecycle ops below.
verify_one_sandbox() { # <label> <ping-budget-ms> [template-id]
	local label="$1" budget="$2" template_id="${3:-$TEMPLATE_ID}"
	local response t0 t1 t2 route code deadline payload
	local policy_file="$GEN_DIR/verify-policy.json"
	t0="$(now_ms)"
	# Egress policy rides on every create (binding -> nft rules), so the
	# chain is exercised each time; /ping is inbound and unaffected.
	jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "example.com"}, {action: "allow", target: "*.opensandbox.ai"}]}' \
		> "$policy_file"
	log "verify ($label): creating a sandbox via the raw API (templateId=$template_id; the timed window is curl-only)"
	# The timed windows wrap HTTP round trips ONLY: an osb invocation here
	# would bill ~0.5s of Python startup to every reading. The CLI surface
	# stays covered by the lifecycle ops and the osb create smoke.
	local payload
	payload="$(jq -n --arg tpl "$template_id" --slurpfile pol "$policy_file" \
		'{templateId: $tpl, timeout: 3600, metadata: {origin: "fast-sandbox-env-verify"}, networkPolicy: $pol[0]}')"
	response="$(server_api POST "/sandboxes" "$payload")" \
		|| fail "POST /sandboxes failed against $SERVER_URL (payload: $(printf '%s' "$payload" | head -c 200))"
	VERIFY_ID="$(printf '%s' "$response" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	t1="$(now_ms)"
	# Availability = /ping 200 through the signed gateway route (port 44772),
	# measured at ~1ms resolution:
	#   phase 1 — route publication, curl-only busy poll (the curl spawn
	#   itself paces ~5ms/attempt; /ping cannot 200 before the route exists,
	#   so phase-1 cadence delays detection, never pollutes the reading);
	#   phase 2 — /ping hammered over a bash /dev/tcp socket: no process
	#   spawn per attempt, EPOCHREALTIME keeps the hot loop fork-free.
	local gw gw_host gw_port line deadline_ms
	gw="${GATEWAY_URL#*://}"; gw="${gw%%/*}"
	gw_host="${gw%%:*}"
	gw_port="${gw##*:}"
	[[ "$gw_port" == "$gw" ]] && gw_port=80
	deadline_ms=$(( t1 / 1000000 + budget ))
	while :; do
		route="$( { server_api GET "/sandboxes/$VERIFY_ID/endpoints/44772" 2>/dev/null || true; } \
			| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
		[[ -n "$route" ]] && break
		if (( $(now_ms) / 1000000 >= deadline_ms )); then
			fail "gateway route for $VERIFY_ID did not publish within ${budget}ms"
		fi
	done
	code=""
	while :; do
		line=""
		# NOTE: no 2>/dev/null here — exec-without-command applies
		# redirections to the CURRENT shell, so that would permanently
		# swallow the script's stderr (every later log/pass goes dark).
		# A refused connect prints one honest bash message instead.
		if exec 3<>"/dev/tcp/$gw_host/$gw_port"; then
			printf 'GET /ping HTTP/1.1\r\nHost: %s\r\nOpenSandbox-Ingress-To: %s\r\nConnection: close\r\n\r\n' \
				"$gw" "$route" >&3
			IFS= read -r -t 1 line <&3 || true
			exec 3>&- 3<&-
		else
			sleep 0.01
		fi
		if [[ "$line" == *" 200 "* ]]; then
			code=200
			break
		fi
		if [[ -n "${EPOCHREALTIME:-}" ]]; then
			now=$((( ${EPOCHREALTIME/./} ) / 1000))
		else
			now=$(( $(now_ms) / 1000000 ))
		fi
		if (( now >= deadline_ms )); then
			fail "execd /ping did not return 200 within ${budget}ms (last status: ${line:-none})"
		fi
	done
	t2="$(now_ms)"
	# jq filter in a variable: literal parens trip older bash's $( ) parser.
	local state raw raw_filter
	raw_filter='{rt:.status.runtime.state,dp:.status.dataPlane.state,infra:[.status.infraComponents[]?|{n:.name,s:.state}],bind:[.status.actionBindings[]?|{h:.handler,s:.state}],ready:(.status.conditions[]?|select(.type=="Ready")|.status)}'
	state="$(osb_api sandbox get "$VERIFY_ID" -o json 2>/dev/null | jq -r '.status.state // empty')"
	raw="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o json 2>/dev/null | jq -c "$raw_filter")"
	log "verify ($label): $VERIFY_ID access via ingress gateway: curl -H \"OpenSandbox-Ingress-To: $route\" $GATEWAY_URL/ping"
	log "verify ($label): $VERIFY_ID create $(( (t1 - t0) / 1000000 ))ms, create->execd /ping 200 $(( (t2 - t1) / 1000000 ))ms, total $(( (t2 - t0) / 1000000 ))ms"
	log "verify ($label): CR at ping: server=$state raw=${raw:-unreachable}"
	pass "execd /ping 200 through the signed gateway route (44772)"
	wait_for "egress policy enforcing (networkPolicy -> egress action binding -> nft)" 120 verify_policy_enforced
	# Policy UPDATE path (raw PUT: the CLI only has merge-style patches).
	local put_body
	put_body="$(jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "github.com"}]}')"
	server_api PUT "/sandboxes/$VERIFY_ID/networkpolicy" "$put_body" >/dev/null \
		|| fail "PUT networkpolicy failed for $VERIFY_ID: $(printf '%s' "$put_body" | head -c 200)"
	wait_for "policy update converged (PUT -> ReplaceActionBindings -> egress)" 120 verify_policy_updated
	# Re-sample the CR state: an early Failed observation may converge late.
	local state_final
	state_final="$(osb_api sandbox get "$VERIFY_ID" -o json 2>/dev/null | jq -r '.status.state // empty')"
	log "verify ($label): $VERIFY_ID CR state final: ${state_final:-unknown}"
}

# Remaining lifecycle surface on one sandbox: get, list, metadata merge-patch
# (raw API — no CLI command), renew via the CLI (TTL-based).
verify_lifecycle_ops() { # <sandbox-id>
	local id="$1" out expected_expires renewed_expires
	out="$(osb_api sandbox get "$id" -o json 2>/dev/null)" || fail "osb sandbox get $id failed"
	[[ "$(printf '%s' "$out" | jq -r '.id')" == "$id" ]] || fail "GET returned wrong id: $out"
	pass "lifecycle: osb sandbox get"

	osb_api sandbox list --page 1 --page-size 50 -o json > "$WORK/last-list.json" 2>"$WORK/last-list.err" \
		|| fail "osb sandbox list failed: $(head -c 400 "$WORK/last-list.err" 2>/dev/null)"
	out="$(cat "$WORK/last-list.json")"
	[[ "$(printf '%s' "$out" | jq -r --arg id "$id" '.items[]?.id | select(. == $id)' | head -1)" == "$id" ]] \
		|| fail "list does not contain $id: $(printf '%s' "$out" | jq -c '.pagination')"
	pass "lifecycle: osb sandbox list (contains the verify sandbox)"

	# JSON Merge Patch (RFC 7396): non-null upserts, null deletes.
	server_api PATCH "/sandboxes/$id/metadata" '{"env":"verify","stage":"lifecycle-ops"}' >/dev/null \
		|| fail "PATCH metadata upsert failed"
	out="$(osb_api sandbox get "$id" -o json 2>/dev/null)"
	[[ "$(printf '%s' "$out" | jq -r '.metadata.env')" == "verify" \
		&& "$(printf '%s' "$out" | jq -r '.metadata.stage')" == "lifecycle-ops" ]] \
		|| fail "metadata upsert not visible: $(printf '%s' "$out" | jq -c '.metadata')"
	server_api PATCH "/sandboxes/$id/metadata" '{"stage":null}' >/dev/null \
		|| fail "PATCH metadata delete failed"
	out="$(osb_api sandbox get "$id" -o json 2>/dev/null)"
	[[ "$(printf '%s' "$out" | jq -r '.metadata.stage')" == "null" \
		&& "$(printf '%s' "$out" | jq -r '.metadata.env')" == "verify" ]] \
		|| fail "metadata delete not visible: $(printf '%s' "$out" | jq -c '.metadata')"
	pass "lifecycle: PATCH metadata (upsert + null-delete via JSON Merge Patch)"

	# CLI renew takes a TTL: assert the expiration advanced and is visible.
	expected_expires="$(osb_api sandbox get "$id" -o json 2>/dev/null | jq -r '.expires_at')"
	out="$(osb_api sandbox renew "$id" --timeout 2h -o json 2>"$WORK/last-renew.err")" \
		|| fail "osb sandbox renew failed: $(head -c 400 "$WORK/last-renew.err" 2>/dev/null)"
	renewed_expires="$(printf '%s' "$out" | jq -r '.expires_at')"
	[[ -n "$renewed_expires" && "$renewed_expires" != "null" && "$renewed_expires" != "$expected_expires" ]] \
		|| fail "renewed expiresAt did not advance: $expected_expires -> $renewed_expires"
	local got_expires
	got_expires="$(osb_api sandbox get "$id" -o json 2>/dev/null | jq -r '.expires_at')"
	# Same instant, two renderings (renew panel str() vs RFC3339 dump):
	# compare epoch seconds, falling back to string equality.
	if [[ "$got_expires" != "$renewed_expires" ]] \
		&& [[ "$(date -u -d "$renewed_expires" +%s 2>/dev/null || true)" \
			!= "$(date -u -d "$got_expires" +%s 2>/dev/null || true)" ]]; then
		fail "renewed expiresAt not visible: expected $renewed_expires got $got_expires"
	fi
	pass "lifecycle: osb sandbox renew 2h ($expected_expires -> $renewed_expires)"
}

# dump_latency_evidence collects the per-create timing instrumentation from
# every component on the create critical path into one greppable file:
# fastlet phase breakdown (fastlet create timing:), fastlet->egress action
# invocations (fastlet action invoke:), egress sidecar SET_BINDING / hook
# steps (timing fields on its subject lines), and fastpath create totals.
# Append-only evidence for create-latency analysis (issue #2323 style).
dump_latency_evidence() {
	local out="$LOGS_DIR/latency-evidence-$(date +%s).txt"
	{
		echo "=== latency evidence $(date -u +%FT%TZ) ==="
		echo
		echo "--- fastlet: create phase + action-invoke timing ---"
		kubectl -n "$RESOURCE_NS" logs -l app=sandbox-fastlet --tail=3000 2>/dev/null \
			| grep -E "fastlet create timing|fastlet action invoke" || true
		echo
		echo "--- egress sidecar: SET_BINDING / hook timing ---"
		kubectl -n "$RESOURCE_NS" logs -l app=sandbox-fastlet -c egress --tail=3000 2>/dev/null \
			| grep -E "deny-first enforced|SET_BINDING applied|data-plane-ready" || true
		echo
		echo "--- server: fsb create served (authoritative control-plane timing) ---"
		kubectl -n "$OSB_NS" logs deploy/opensandbox-server --tail=3000 2>/dev/null \
			| grep "fsb create served" | tail -30 || true
		echo
		echo "--- fastpath create totals (controller) ---"
		kubectl -n "$NS" logs deploy/fast-sandbox-controller --tail=3000 2>/dev/null \
			| grep "fastpath sandbox created" | tail -30 || true
	} > "$out" 2>&1 || true
	log "latency evidence: $out"
	# CI runners discard $WORK between runs, so the content must also land
	# in the job log to be analyzable.
	cat "$out"
}

opensandbox_verify() {
	# Optional template override: the overlaybd second pass calls this with
	# the OCI template id; the default stays the native template. Cold create
	# pulls the golden image through DART (native) or the registry over ublk
	# (overlaybd); warm creates: the second may still pull on the other node
	# via its DART peer; after both nodes cache the set, creates are
	# sub-second (native) / attach cached devices (overlaybd).
	local template_id="${1:-$TEMPLATE_ID}"
	log "verify (end-to-end): template=$template_id"
	local ids=() id label index
	verify_one_sandbox "cold #1" 600000 "$template_id"
	ids+=("$VERIFY_ID")
	verify_one_sandbox "warm #2" 600000 "$template_id"
	ids+=("$VERIFY_ID")
	for index in 3 4 5 6; do
		verify_one_sandbox "warm #$index" 120000 "$template_id"
		ids+=("$VERIFY_ID")
	done
	pass "end-to-end: opensandbox CLI -> server -> FastPath -> fastlet -> sandbox execd -> gateway route OK (6 sandboxes)"
	dump_latency_evidence
	verify_lifecycle_ops "${ids[0]}"
	for id in "${ids[@]}"; do
		VERIFY_ID="$id"
		osb_api sandbox kill "$id" >/dev/null 2>&1 \
			|| log "verify cleanup: kill failed; remove $id manually"
	done
	for id in "${ids[@]}"; do
		VERIFY_ID="$id"
		wait_for "verify sandbox $id deleted" 120 verify_sandbox_gone
	done
	pass "verify sandboxes cleaned up"
}

# --- stage: pause / resume (opensandbox CLI -> FastPath checkpoint) -------------

# Fresh signed route + one GET: doubles as the "runtime actually serving" probe.
execd_ping_ok() {
	local route code
	route="$(osb_api sandbox endpoint "$VERIFY_ID" --port 44772 -o json 2>/dev/null \
		| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
	[[ -n "$route" ]] || return 1
	code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' \
		-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" 2>/dev/null || true)"
	[[ "$code" == "200" ]]
}

sandbox_state_is() { # <state>
	[[ "$(osb_api sandbox get "$VERIFY_ID" -o json 2>/dev/null | jq -r '.status.state // empty')" == "$1" ]]
}

sandbox_running() { sandbox_state_is Running; }
sandbox_paused() { sandbox_state_is Paused; }
sandbox_state_get() {
	osb_api sandbox get "$VERIFY_ID" -o json 2>/dev/null | jq -r '.status.state // empty'
}

pause_resume_verify() {
	# Optional template override (the overlaybd second pass); default native.
	# Accepted + poll: Paused == checkpoint durable + capacity released;
	# resume advances the route generation, so /ping needs a fresh route.
	local template_id="${1:-$TEMPLATE_ID}"
	local created t0 t1 t2 t3
	log "verify (pause): creating a sandbox via the opensandbox CLI (templateId=$template_id)"
	t0="$(now_ms)"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-pause --skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "pause target Running" 150 sandbox_running
	t1="$(now_ms)"
	log "verify (pause): $VERIFY_ID create->Running $(( (t1 - t0) / 1000000 ))ms"

	wait_for "pre-pause execd /ping 200 through the gateway" 100 execd_ping_ok

	log "verify (pause): osb sandbox pause $VERIFY_ID"
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 \
		|| fail "pause failed for $VERIFY_ID"
	# Paused is durable-first: the checkpoint must be complete in the artifact
	# store before the state is reported; the dump itself keeps serving.
	wait_for "poll GET until Paused (checkpoint durable, capacity released)" 240 sandbox_paused
	t2="$(now_ms)"
	log "verify (pause): $VERIFY_ID pause->Paused (checkpoint durable) $(( (t2 - t1) / 1000000 ))ms"
	if execd_ping_ok; then
		fail "paused sandbox still serves /ping through the gateway"
	fi
	pass "paused: execd /ping no longer served (runtime released, signed route rejected at the gateway)"

	log "verify (pause): osb sandbox resume $VERIFY_ID"
	t2="$(now_ms)"
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 \
		|| fail "resume failed for $VERIFY_ID"
	wait_for "poll GET until Running (checkpoint restored)" 240 sandbox_running
	t3="$(now_ms)"
	log "verify (pause): $VERIFY_ID resume->Running (checkpoint restored) $(( (t3 - t2) / 1000000 ))ms"
	wait_for "post-resume execd /ping 200 through a fresh route" 100 execd_ping_ok
	pass "pause/resume round-trip timings: pause->Paused $(( (t2 - t1) / 1000000 ))ms, resume->Running $(( (t3 - t2) / 1000000 ))ms (opensandbox CLI -> FastPath -> artifact store)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "pause/resume sandbox deleted" 120 verify_sandbox_gone
}

# --- stage: vault lifecycle (OSEP-0022: egress-held vault over the relay) --------

# Status-code-returning variant of server_api for negative-path assertions
# (curl -f hides the status).
server_api_status() { # method path [json-body]
	local method="$1" path="$2" body="${3:-}" out
	if [[ -n "$body" ]]; then
		out="$(curl -sS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-H "Content-Type: application/json" -d "$body" -w '\n%{http_code}' "$SERVER_URL$path" 2>/dev/null)"
	else
		out="$(curl -sS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-w '\n%{http_code}' "$SERVER_URL$path" 2>/dev/null)"
	fi
	printf '%s' "${out##*$'\n'}"
}

# The vault stage's sandbox must ride the subject-scoped egress log markers
# (export/import/registration), so every log assertion goes through this.
vault_log_contains() { # <fixed-substring>
	kubectl -n "$RESOURCE_NS" logs -l app=sandbox-fastlet -c egress --tail=800 2>/dev/null \
		| grep -qF "$1"
}

vault_revision_is() { # <expected-revision>
	local out
	out="$(server_api GET "/sandboxes/$VERIFY_ID/credential-vault" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.revision // empty')" == "$1" ]]
}

vault_lifecycle_verify() {
	local template_id="${1:-$TEMPLATE_ID}"
	local created vault_body ack rev sandbox_uid
	log "verify (vault): creating a sandbox via the opensandbox CLI (templateId=$template_id)"
	# The policy rides the create: a policy-less create produces no egress
	# binding at all, so the subject would never register.
	jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "ext.test"}]}' \
		> "$GEN_DIR/vault-policy.json"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-vault \
		--network-policy-file "$GEN_DIR/vault-policy.json" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "vault verify: sandbox create failed"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	wait_for "vault sandbox Running" 150 sandbox_running
	sandbox_uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$sandbox_uid" ]] || fail "vault verify: sandbox CR carried no uid"
	# Registered and activated before the vault POST, else the relay 404s.
	# (The "(state=active)" re-delivery only follows a policy UPDATE; a
	# create-carried policy activates without it.)
	wait_for "vault subject registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: data-plane-ready, policy active"

	# 1) create-only: ack carries the revision; duplicate POST is 409.
	vault_body="$(jq -n '{credentials: [{name: "tok", source: {type: "inline", value: "sekrit-vault-e2e"}}], bindings: [{name: "ext", match: {hosts: ["ext.test"]}, auth: {type: "apiKey", name: "X-Api-Key", credential: "tok"}}]}')"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "vault verify: POST credential-vault failed"
	rev="$(printf '%s' "$ack" | jq -r '.revision // empty')"
	[[ "$rev" =~ ^[0-9]+$ ]] || fail "vault verify: create ack carried no revision: $(printf '%s' "$ack" | head -c 200)"
	pass "vault created over the relay (ack revision $rev)"
	if server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body" >/dev/null 2>&1; then
		fail "vault verify: duplicate POST succeeded (create-only must 409)"
	fi
	[[ "$(server_api_status POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" == "409" ]] \
		|| fail "vault verify: duplicate POST was not 409"
	pass "duplicate POST rejected (create-only 409)"

	# 2) pause/resume carries the real vault: export -> bundle -> import,
	#    with the relay GET reporting the same revision.
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 || fail "vault verify: pause failed"
	wait_for "vault sandbox Paused (checkpoint durable)" 240 sandbox_paused
	wait_for "vault exported into the checkpoint bundle" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault exported to snapshot bundle"
	pass "pause exported the vault bundle (subject sealed)"
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 || fail "vault verify: resume failed"
	wait_for "vault sandbox Running (restored)" 240 sandbox_running
	# The pause's REMOVE_BINDING purges the subject, so the restore import
	# lands through the unregistered branch and the file reloads at the
	# SET_BINDING replay (the reload line carries the restored revision).
	wait_for "vault bundle reloaded at re-registration" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault reloaded from file"
	wait_for "vault revision preserved across pause/resume" 120 vault_revision_is "$rev"
	pass "vault survived pause/resume (revision $rev preserved)"

	# 3) egress-restart recovery: crash the worker (fresh instanceId ->
	#    SET_BINDING replay -> vault file reload by incarnation).
	local pod restarted=0
	for pod in $(pool_pods); do
		if kubectl -n "$RESOURCE_NS" exec "$pod" -c egress -- sh -c 'command -v pkill >/dev/null' 2>/dev/null; then
			kubectl -n "$RESOURCE_NS" exec "$pod" -c egress -- pkill -f '/opt/opensandbox-egress/egress' >/dev/null 2>&1 || true
			restarted=1
		else
			log "verify (vault): egress restart skipped on ${pod##*/} (pkill unavailable)"
		fi
	done
	if [[ "$restarted" == 1 ]]; then
		wait_for "pool egress containers back up" 180 egress_containers_ready
		wait_for "vault reloaded after egress restart" 240 vault_revision_is "$rev"
		pass "vault reloaded after egress restart (revision $rev preserved)"
	fi

	# 4) delete + re-create.
	server_api DELETE "/sandboxes/$VERIFY_ID/credential-vault" >/dev/null \
		|| fail "vault verify: DELETE failed"
	[[ "$(server_api_status GET "/sandboxes/$VERIFY_ID/credential-vault")" == "404" ]] \
		|| fail "vault verify: GET after DELETE was not 404"
	pass "vault deleted (GET 404)"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "vault verify: re-create after DELETE failed"
	rev="$(printf '%s' "$ack" | jq -r '.revision // empty')"
	[[ "$rev" == "1" ]] || fail "vault verify: fresh vault revision is $rev, expected 1"
	pass "vault re-created (fresh revision $rev)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "vault sandbox deleted" 120 verify_sandbox_gone
}

# --- stage: MITM CA chain verify (OSEP-0022: baked CA + injection + pinning) ----
# NOTE: verify/vault.sh carries the split-layout copy of this stage (plus its
# helpers); keep the two in sync until the env.sh split is completed and the
# verify scripts become the single home.

# Guest-side HTTPS request to the echo endpoint via `osb command run` (the
# only sandbox-reachable surface). `curl --resolve` pins the SNI to the echo
# domain — the domain is deliberately unresolvable DNS, and the policy
# carries the IP-allow rule the resolved flow needs; the egress DNATs the
# allowed flow to the shared mitmdump, which matches the vault binding by
# SNI. TLS verification runs against the template-baked CA (no -k anywhere).
# The guest-side stderr (curl/CLI error text — the first diagnostic for a
# broken TLS chain) is kept per attempt at $LOGS_DIR/mitm-guest-curl.err for
# the failure dump.
mitm_ca_guest_echo() {
	# The CLI joins command-run words with shlex.quote into one shell line —
	# the curl invocation must ride as separate words (one quoted string
	# would be executed as a single program name and exit 127).
	osb_api command run "$VERIFY_ID" -- \
		curl -fsS -m 20 --resolve "$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT:$MITM_ECHO_IP" \
		"https://$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT/echo" \
		2>"$LOGS_DIR/mitm-guest-curl.err"
}

# osb_command_succeeds <command words...>: wait_for probe variant — the
# guest command must exit 0 (a failed execd command makes the CLI exit
# non-zero). stderr lands in the same per-attempt diagnostics file as
# mitm_ca_guest_echo.
osb_command_succeeds() {
	osb_api command run "$VERIFY_ID" -- "$@" >/dev/null 2>"$LOGS_DIR/mitm-guest-curl.err"
}

# mitm_ca_guest_header_is <key> <value>: the echo reflected the exact header.
# Scans line-by-line: the CLI stream may carry non-JSON framing.
mitm_ca_guest_header_is() { # <lowercase-key> <expected-value>
	local out line
	out="$(mitm_ca_guest_echo)" || return 1
	while IFS= read -r line; do
		[[ -n "$line" ]] || continue
		printf '%s' "$line" | jq -e --arg k "$1" --arg v "$2" '.[$k] == $v' >/dev/null 2>&1 && return 0
	done <<EOF
$out
EOF
	return 1
}

mitm_ca_chain_verify() {
	# Optional template override (the overlaybd second pass); default native.
	local template_id="${1:-$TEMPLATE_ID}"
	# Fingerprint pins live in a file precisely because up and the verify
	# actions are separate invocations (same pattern as require_template_id).
	load_mitm_ca_fingerprint
	[[ -n "$MITM_CA_FINGERPRINT" ]] \
		|| die "no pinned MITM CA fingerprint at $MITM_CA_FINGERPRINT_FILE (run up first; mitm_ca_up stores it there)"
	load_mitm_echo_endpoint
	local created sandbox_uid ack echo_secret
	log "verify (mitm-ca): fingerprint pinning (pinned Secret == egress /healthz, plus the manifest lineage on the S3 publication path)"
	local healthz_fp lineage_fp manifest_ref
	# Guarded so a failed probe lands on the crafted mismatch message (and
	# its diagnostics) instead of an ERR-trap abort.
	healthz_fp="$(egress_mitm_ca_fingerprint)" || true
	[[ "$healthz_fp" == "$MITM_CA_FINGERPRINT" ]] \
		|| fail "mitm-ca verify: egress /healthz fingerprint is '${healthz_fp:-empty}', want pinned $MITM_CA_FINGERPRINT"
	# The manifest lineage leg is only readable on the S3 publication path:
	# an oci:// manifestRef lives in the in-cluster registry, out of rc's
	# reach. There the pool-side pinning plus the behavioral chain below
	# (the guest accepts the MITM certificate only because it chains to the
	# template-baked CA) carry the assertion.
	manifest_ref="$(osb_api template get "$template_id" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')" || true
	if [[ "$manifest_ref" == s3://* ]]; then
		lineage_fp="$(template_mitm_lineage_fingerprint "$template_id")"
		[[ "$lineage_fp" == "$MITM_CA_FINGERPRINT" ]] \
			|| fail "mitm-ca verify: template manifest platform.egressMitmCA fingerprint is '${lineage_fp:-empty}', want pinned $MITM_CA_FINGERPRINT"
		pass "template↔pool CA pinning: manifest platform.egressMitmCA == egress /healthz == pinned (${MITM_CA_FINGERPRINT:0:16}…)"
	else
		pass "template↔pool CA pinning: egress /healthz == pinned (${MITM_CA_FINGERPRINT:0:16}…); ${manifest_ref:-no manifest_ref} — guest-side chain proven behaviorally below"
	fi

	# Sandbox policy: the domain allow covers the vault binding (binding
	# validation requires the host be policy-allowed), the IP allow covers
	# the --resolve flow at the nft layer (no DNS query ever happens).
	jq -n --arg ip "$MITM_ECHO_IP" --arg domain "$MITM_ECHO_DOMAIN" \
		'{defaultAction: "deny", egress: [{action: "allow", target: $ip}, {action: "allow", target: $domain}]}' \
		> "$GEN_DIR/mitm-ca-policy.json"
	log "verify (mitm-ca): creating a sandbox via the opensandbox CLI (templateId=$template_id, echo=$MITM_ECHO_IP)"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-mitm-ca \
		--network-policy-file "$GEN_DIR/mitm-ca-policy.json" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "mitm-ca verify: sandbox create failed"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "mitm-ca verify: create response carried no id"
	wait_for "mitm-ca sandbox Running" 300 sandbox_running
	sandbox_uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$sandbox_uid" ]] || fail "mitm-ca verify: sandbox CR carried no uid"
	wait_for "mitm-ca subject registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: data-plane-ready, policy active"

	# Negative control BEFORE the vault exists: the flow is allowed and
	# passes through the shared mitmdump opaquely (unbound SNI), TLS is
	# verified end-to-end against the template-baked CA — and no credential
	# header appears. Proves the header in the positive case comes from the
	# vault, not from anything ambient.
	wait_for "pre-vault guest HTTPS reaches the echo (passthrough, baked-CA verified)" 120 \
		osb_command_succeeds curl -fsS -m 20 \
		--resolve "$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT:$MITM_ECHO_IP" \
		"https://$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT/echo"
	local pre_vault_out
	pre_vault_out="$(mitm_ca_guest_echo)" \
		|| fail "mitm-ca verify: pre-vault guest HTTPS request failed (cannot assert header absence)"
	if printf '%s' "$pre_vault_out" | grep -qi 'x-api-key'; then
		fail "mitm-ca verify: x-api-key present before any vault binding existed"
	fi
	pass "pre-vault: guest HTTPS passthrough works, no credential injected"

	# Per-subject vault binding: SNI echo.test -> X-Api-Key.
	echo_secret="mitm-ca-e2e-$(openssl rand -hex 8)"
	local vault_body
	vault_body="$(jq -n --arg v "$echo_secret" --arg domain "$MITM_ECHO_DOMAIN" \
		'{credentials: [{name: "tok", source: {type: "inline", value: $v}}],
		  bindings: [{name: "echo", match: {hosts: [$domain]}, auth: {type: "apiKey", name: "X-Api-Key", credential: "tok"}}]}')"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "mitm-ca verify: POST credential-vault failed"
	[[ "$(printf '%s' "$ack" | jq -r '.revision // empty')" =~ ^[0-9]+$ ]] \
		|| fail "mitm-ca verify: vault create ack carried no revision: $(printf '%s' "$ack" | head -c 200)"
	pass "vault bound to SNI $MITM_ECHO_DOMAIN (X-Api-Key) over the relay"

	wait_for "guest HTTPS reflects the injected X-Api-Key (MITM decrypt + vault by SNI)" 120 \
		mitm_ca_guest_header_is "x-api-key" "$echo_secret"
	pass "credential injection observed by the echo endpoint on a guest-originated HTTPS request"

	# Pause/resume: trust travels with the snapshot (the baked CA is part of
	# the rootfs) and the vault is carried in the bundle — the same request
	# must keep working end to end after restore.
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 || fail "mitm-ca verify: pause failed"
	wait_for "mitm-ca sandbox Paused (checkpoint durable)" 240 sandbox_paused
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 || fail "mitm-ca verify: resume failed"
	wait_for "mitm-ca sandbox Running (restored)" 240 sandbox_running
	wait_for "restored subject re-registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault reloaded from file"
	wait_for "post-resume guest HTTPS still MITM-injected (baked CA + vault survive the snapshot)" 180 \
		mitm_ca_guest_header_is "x-api-key" "$echo_secret"
	pass "paused/restored sandbox keeps passing TLS verification against the baked CA (injection intact)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "mitm-ca sandbox deleted" 120 verify_sandbox_gone
}



# --- stage: idle-pause / wake-on-access verify (OSEP-0024 profile) --------------

# Poll the gateway until /ping returns 200 (marked 503s tolerated; the
# resume is never canceled). Fails on budget exhaustion.
idle_wake_wait() { # <label> [sandbox_id]
	local label="$1" id="${2:-$VERIFY_ID}" code
	local deadline=$(( $(now_ms) + 600000000000 )) wake_seen=0 next_state_log=0
	while :; do
		code="$(idle_ping_code "$id")"
		case "$code" in
			200) break ;;
			wake-503) wake_seen=1 ;;
		esac
		if (( $(now_ms) >= next_state_log )); then
			log "verify (idle-pause): $label still waiting (last=$code, state=$(sandbox_state_get))"
			next_state_log=$(( $(now_ms) + 30000000000 ))
		fi
		[[ "$(now_ms)" -ge "$deadline" ]] && fail "$label: wake /ping never returned 200 (last=$code)"
		sleep 1
	done
}

# Gateway answer for one /ping attempt: "200", "wake-503", or "other-<code>".
idle_ping_code() { # [sandbox_id] (default $VERIFY_ID)
	local id="${1:-$VERIFY_ID}" route headers code marker
	route="$(osb_api sandbox endpoint "$id" --port 44772 -o json 2>/dev/null \
		| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
	[[ -n "$route" ]] || { echo "noroute"; return 0; }
	headers="$(mktemp)"
	curl -sS -m 10 -D "$headers" -o /dev/null \
		-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" >/dev/null 2>&1 || true
	code="$(tr -d '\r' < "$headers" | head -1 | awk '{print $2}')" || true
	marker="$(tr -d '\r' < "$headers" | grep -i '^OpenSandbox-Wake:' | awk '{print $2}')" || true
	rm -f "$headers"
	if [[ "$code" == "200" ]]; then
		echo "200"
	elif [[ "$code" == "503" && "$marker" == "expired" ]]; then
		echo "wake-503"
	else
		echo "other-${code:-none}"
	fi
}

idle_pause_verify() {
	# Raw curl create: the CLI has no lifecycle.idlePolicy parameter yet.
	# Optional template override (the overlaybd second pass); default native.
	[[ "$IDLE_PAUSE" == "1" ]] || { log "idle-pause profile off (IDLE_PAUSE=0); skipping verify"; return 0; }
	local template_id="${1:-$TEMPLATE_ID}"
	log "verify (idle-pause): template=$template_id"
	local created t0 t1 code i
	local x=30
	local rounds="${IDLE_PAUSE_ROUNDS:-3}"
	[[ "$rounds" =~ ^[1-9][0-9]*$ ]] || die "IDLE_PAUSE_ROUNDS must be a positive integer (got $rounds)"
	log "verify (idle-pause): creating an opted-in sandbox (idleTimeoutSeconds=$x, rounds=$rounds) via the lifecycle API"
	t0="$(now_ms)"
	created="$(server_api POST /sandboxes "$(jq -n \
		--arg template "$template_id" \
		--argjson timeout 3600 --argjson x "$x" \
		'{templateId: $template, timeout: $timeout,
		  lifecycle: {idlePolicy: {idleTimeoutSeconds: $x, onIdle: "pause", wakeOnAccess: true}},
		  metadata: {origin: "fast-sandbox-env-idle-pause"}}')" 2>/dev/null)" \
		|| fail "idle-pause create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "idle-pause target Running" 300 sandbox_running
	log "verify (idle-pause): $VERIFY_ID create->Running $(( ($(now_ms) - t0) / 1000000 ))ms"

	wait_for "pre-idle execd /ping 200 through the gateway (records activity)" 100 execd_ping_ok

	local -a pause_ms=() wake_ms=() attempts=()
	for ((i = 1; i <= rounds; i++)); do
		t0="$(now_ms)"
		wait_for "round $i/$rounds auto-paused within X + sweep + checkpoint" 240 sandbox_paused
		pause_ms+=("$(( ($(now_ms) - t0) / 1000000 ))")
		pass "round $i/$rounds: auto-paused in ${pause_ms[-1]}ms"

		t1="$(now_ms)"
		idle_wake_wait "round $i wake"
		wake_ms+=("$(( ($(now_ms) - t1) / 1000000 ))")
		attempts+=("$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" \
			-o jsonpath='{.status.placement.attempt}' 2>/dev/null || echo '?')")
		pass "round $i/$rounds: woke in ${wake_ms[-1]}ms (placement.attempt=${attempts[-1]})"
	done

	local joined_pause joined_wake joined_att
	printf -v joined_pause '%sms/' "${pause_ms[@]}"
	printf -v joined_wake '%sms/' "${wake_ms[@]}"
	printf -v joined_att '%s/' "${attempts[@]}"
	pass "idle-pause ${rounds} rounds: pause=${joined_pause%?} wake=${joined_wake%?} placement.attempt=${joined_att%?}"

	local main_id="$VERIFY_ID" created2 id2 payload
	log "verify (idle-pause): scenario wakeOnAccess=false"
	created2="$(server_api POST /sandboxes "$(jq -n \
		--arg template "$template_id" --argjson timeout 3600 --argjson x "$x" \
		'{templateId: $template, timeout: $timeout,
		  lifecycle: {idlePolicy: {idleTimeoutSeconds: $x, onIdle: "pause", wakeOnAccess: false}},
		  metadata: {origin: "fast-sandbox-env-idle-pause"}}')" 2>/dev/null)" \
		|| fail "wakeOnAccess=false create failed"
	id2="$(printf '%s' "$created2" | jq -r '.id')"
	[[ -n "$id2" && "$id2" != "null" ]] || fail "wakeOnAccess=false create carried no id"
	VERIFY_ID="$id2"
	wait_for "wakeOnAccess=false target Running" 300 sandbox_running
	wait_for "wakeOnAccess=false auto-paused" 240 sandbox_paused
	code="$(idle_ping_code "$id2")"
	[[ "$code" == "200" || "$code" == "wake-503" ]] \
		&& fail "wakeOnAccess=false sandbox was woken by traffic (code=$code)"
	sleep 10
	sandbox_paused || fail "wakeOnAccess=false sandbox changed state without resume API"
	osb_api sandbox resume "$id2" --skip-health-check >/dev/null 2>&1 || fail "manual resume failed"
	wait_for "wakeOnAccess=false resumed manually" 240 sandbox_running
	pass "wakeOnAccess=false: traffic never woke it, manual resume did"
	osb_api sandbox kill "$id2" >/dev/null 2>&1 || true
	wait_for "wakeOnAccess=false sandbox deleted" 120 verify_sandbox_gone
	VERIFY_ID="$main_id"

	log "verify (idle-pause): scenario redis down"
	# The previous scenario may have left it Paused.
	idle_wake_wait "pre-redis-down wake"
	t0="$(now_ms)"
	kubectl -n "$OSB_NS" scale deploy/idle-pause-redis --replicas=0 >/dev/null
	while sandbox_running; do
		(( $(now_ms) - t0 >= 90000000000 )) && break
		sleep 5
	done
	sandbox_running || fail "sandbox paused while Redis was scaled down"
	pass "redis down: no pauses while Redis unreachable"
	kubectl -n "$OSB_NS" scale deploy/idle-pause-redis --replicas=1 >/dev/null
	wait_for "redis back ready" 120 \
		kubectl -n "$OSB_NS" rollout status deploy/idle-pause-redis --timeout=10s
	wait_for "tombstone pause after redis recovery" 300 sandbox_paused
	pass "redis recovery: missing-key tombstone paused after the full idle window"

	idle_wake_wait "post-flushall wake"
	t0="$(now_ms)"
	kubectl -n "$OSB_NS" exec deploy/idle-pause-redis -- redis-cli FLUSHALL >/dev/null 2>&1 \
		|| fail "FLUSHALL failed"
	while :; do
		if sandbox_paused; then
			(( $(now_ms) - t0 < (x - 5) * 1000000000 )) \
				&& fail "FLUSHALL accelerated the pause ($(( ($(now_ms) - t0) / 1000000 ))ms after flush)"
			break
		fi
		(( $(now_ms) - t0 >= 300000000000 )) && fail "no pause within budget after FLUSHALL"
		sleep 2
	done
	pass "flushall: pause delayed to a full idle window after data loss ($(( ($(now_ms) - t0) / 1000000 ))ms)"


	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "idle-pause sandbox deleted" 120 verify_sandbox_gone
}

# --- stage: snapshot (opensandbox CLI -> SandboxSnapshot CR -> restore) ----------

# Ready once the server watcher converges the row from the SandboxSnapshot
# CR (Succeeded + template index published).
snapshot_ready() { # <snapshot-id>
	[[ "$(osb_api snapshot get "$1" -o json 2>/dev/null | jq -r '.status.state // empty')" == "Ready" ]]
}

# Terminal (Ready or Failed): the pause-window fence resolves any
# accepted-but-conflicting snapshot one way or the other.
snapshot_terminal() { # <snapshot-id>
	local state
	state="$(osb_api snapshot get "$1" -o json 2>/dev/null | jq -r '.status.state // empty')"
	[[ "$state" == "Ready" || "$state" == "Failed" ]]
}

snapshot_verify() {
	# Full round trip on the live stack: snapshot a Running sandbox, watch it
	# to Ready, restore a NEW sandbox from the snapshotId and boot it. Also
	# covers re-entry fencing, source-sandbox survival, and a repeat snapshot.
	# Optional template override (the overlaybd second pass); default native.
	local template_id="${1:-$TEMPLATE_ID}"
	local created out source_id snapshot_id snapshot_id2 restore_id reentry_out reentry_code
	local t0 t1 t2 t3 t4
	t0="$(now_ms)"
	log "verify (snapshot): creating the source sandbox via the opensandbox CLI (templateId=$template_id)"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-snapshot --skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	source_id="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$source_id" && "$source_id" != "null" ]] || fail "create response carried no id"
	VERIFY_ID="$source_id"
	wait_for "snapshot source sandbox Running" 300 sandbox_running
	wait_for "pre-snapshot execd /ping 200 through the gateway" 100 execd_ping_ok
	t1="$(now_ms)"
	log "verify (snapshot): source sandbox $source_id create->Running $(( (t1 - t0) / 1000000 ))ms"

	# 1. Snapshot create returns Creating; the row converges from the
	# SandboxSnapshot CR via the server watcher.
	log "verify (snapshot): osb snapshot create $source_id --name env-verify"
	t1="$(now_ms)"
	out="$(osb_api snapshot create "$source_id" --name env-verify -o json 2>/dev/null)" \
		|| fail "snapshot create failed for $source_id"
	SNAPSHOT_ID="$(printf '%s' "$out" | jq -r '.id')"
	[[ -n "$SNAPSHOT_ID" && "$SNAPSHOT_ID" != "null" ]] || fail "snapshot create carried no id"
	[[ "$(printf '%s' "$out" | jq -r '.status')" == "Creating" ]] \
		|| fail "snapshot create did not return Creating: $(printf '%s' "$out" | head -c 300)"

	# 2. Re-entry during the dump window: FastPath fences with 409 once the
	# CR is cache-visible; with cache lag it is accepted (202) and the
	# pause window resolves it to a terminal phase later.
	reentry_out="$(curl -sS -m 60 -w '\n%{http_code}' -X POST \
		-H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" -H "Content-Type: application/json" \
		-d '{"name":"env-verify-reentry"}' "$SERVER_URL/sandboxes/$source_id/snapshots" 2>/dev/null || true)"
	reentry_code="$(printf '%s' "$reentry_out" | tail -n1)"
	reentry_out="$(printf '%s' "$reentry_out" | sed '$d')"
	case "$reentry_code" in
		409)
			pass "snapshot: re-entry rejected by the fence (409)"
			;;
		202)
			SNAPSHOT_EXTRA="$(printf '%s' "$reentry_out" | jq -r '.id' 2>/dev/null || true)"
			[[ -n "$SNAPSHOT_EXTRA" && "$SNAPSHOT_EXTRA" != "null" ]] \
				|| fail "re-entry snapshot POST returned 202 without an id: $(printf '%s' "$reentry_out" | head -c 300)"
			log "verify (snapshot): re-entry accepted during fence cache lag ($SNAPSHOT_EXTRA); terminal outcome asserted below"
			;;
		*)
			fail "re-entry snapshot POST returned unexpected HTTP ${reentry_code:-none}: $(printf '%s' "$reentry_out" | head -c 300)"
			;;
	esac

	wait_for "poll osb snapshot get $SNAPSHOT_ID until Ready (watcher -> SandboxSnapshot CR -> store index)" 300 snapshot_ready "$SNAPSHOT_ID"
	t2="$(now_ms)"
	log "verify (snapshot): $SNAPSHOT_ID snapshot create->Ready $(( (t2 - t1) / 1000000 ))ms"
	pass "snapshot: create -> Creating -> watcher -> Ready"

	# 3. Source survival: Running + /ping again after the terminal phase.
	VERIFY_ID="$source_id"
	wait_for "source sandbox Running again after the snapshot" 120 sandbox_running
	wait_for "source sandbox execd /ping 200 after the snapshot" 100 execd_ping_ok
	pass "snapshot: source sandbox survived (Running + /ping 200)"

	# 3b. An accepted re-entry snapshot must reach a terminal phase; a stuck
	# one would block all future snapshots via the re-entry fence.
	if [[ -n "$SNAPSHOT_EXTRA" ]]; then
		wait_for "re-entry snapshot $SNAPSHOT_EXTRA reaches a terminal phase" 150 snapshot_terminal "$SNAPSHOT_EXTRA"
		log "verify (snapshot): re-entry snapshot $SNAPSHOT_EXTRA terminal: $(osb_api snapshot get "$SNAPSHOT_EXTRA" -o json 2>/dev/null | jq -r '.status.state')"
		pass "snapshot: accepted re-entry snapshot resolved to a terminal phase"
	fi

	# 4. Second snapshot once the first is terminal: fresh id.
	t3="$(now_ms)"
	out="$(osb_api snapshot create "$source_id" --name env-verify-2 -o json 2>/dev/null)" \
		|| fail "second snapshot create failed for $source_id"
	snapshot_id2="$(printf '%s' "$out" | jq -r '.id')"
	[[ -n "$snapshot_id2" && "$snapshot_id2" != "null" && "$snapshot_id2" != "$SNAPSHOT_ID" ]] \
		|| fail "second snapshot did not produce a distinct id: $snapshot_id2"
	SNAPSHOT_ID2="$snapshot_id2"
	wait_for "poll osb snapshot get $snapshot_id2 until Ready" 300 snapshot_ready "$snapshot_id2"
	t4="$(now_ms)"
	log "verify (snapshot): $snapshot_id2 second snapshot create->Ready $(( (t4 - t3) / 1000000 ))ms"
	pass "snapshot: repeated snapshot of the same sandbox -> Ready (distinct id)"

	# 5. Listing: sandboxId scoping returns both required snapshots, both
	# Ready (an accepted re-entry snapshot may add a third, terminal row).
	out="$(osb_api snapshot list --sandbox-id "$source_id" --page-size 50 -o json 2>"$WORK/last-snapshot-list.err")" \
		|| fail "snapshot list failed for $source_id: $(head -c 400 "$WORK/last-snapshot-list.err" 2>/dev/null)"
	[[ "$(printf '%s' "$out" | jq -r --arg id "$SNAPSHOT_ID" --arg id2 "$snapshot_id2" \
		'[.items[] | select((.id == $id or .id == $id2) and .status.state == "Ready")] | length')" == "2" ]] \
		|| fail "snapshot list does not contain both required snapshots as Ready: $(printf '%s' "$out" | head -c 400)"
	if [[ -n "$SNAPSHOT_EXTRA" ]]; then
		[[ "$(printf '%s' "$out" | jq -r --arg id "$SNAPSHOT_EXTRA" \
			'[.items[] | select(.id == $id)] | length')" == "1" ]] \
			|| fail "accepted re-entry snapshot $SNAPSHOT_EXTRA missing from the list"
	fi
	pass "snapshot: list scoped by sandboxId contains the snapshots (Ready)"

	# 6. Restore: the row resolves to the published template index key; the
	# resource flags restate the pool profile (firecracker pool).
	local restored
	log "verify (snapshot): osb sandbox create --snapshot-id $SNAPSHOT_ID (pool profile restated)"
	t3="$(now_ms)"
	restored="$(osb_api sandbox create --snapshot-id "$SNAPSHOT_ID" --timeout 1h \
		--resource cpu=1 --resource memory=512Mi --resource pids=128 \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create (snapshotId=$SNAPSHOT_ID) failed"
	restore_id="$(printf '%s' "$restored" | jq -r '.id')"
	[[ -n "$restore_id" && "$restore_id" != "null" ]] || fail "restore create carried no id"
	VERIFY_ID="$restore_id"
	wait_for "restored sandbox Running" 600 sandbox_running
	t4="$(now_ms)"
	wait_for "restored execd /ping 200 through the gateway" 300 execd_ping_ok
	log "verify (snapshot): restored $restore_id create->Running $(( (t4 - t3) / 1000000 ))ms"
	pass "snapshot: restore -> sandbox boots the published artifact set -> execd /ping OK"

	# 7. Cleanup; the server forwards artifact deletion per snapshot row.
	osb_api sandbox kill "$restore_id" >/dev/null 2>&1 \
		|| log "verify cleanup: kill $restore_id failed"
	wait_for "restored sandbox deleted" 120 verify_sandbox_gone
	VERIFY_ID="$source_id"
	osb_api sandbox kill "$source_id" >/dev/null 2>&1 \
		|| log "verify cleanup: kill $source_id failed"
	wait_for "snapshot source sandbox deleted" 120 verify_sandbox_gone
	for snapshot_id in "$SNAPSHOT_ID" "$SNAPSHOT_ID2" "$SNAPSHOT_EXTRA"; do
		[[ -n "$snapshot_id" ]] || continue
		osb_api snapshot delete "$snapshot_id" >/dev/null 2>&1 \
			|| log "verify cleanup: snapshot delete $snapshot_id failed"
	done
	pass "snapshot: cleanup (restored + source sandboxes, both snapshot rows)"
}

# --- stage: python sdk e2e (tests/python/tests/test_fsb_e2e.py) -----------------

sdk_e2e() {
	kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
		|| die "cluster $KIND_CLUSTER is not up (run up first)"
	curl -fsS -m 5 "$SERVER_URL/health" >/dev/null 2>&1 \
		|| die "server $SERVER_URL is not reachable (run up first)"
	command -v uv >/dev/null 2>&1 \
		|| die "uv is required on PATH (https://docs.astral.sh/uv/ — curl -LsSf https://astral.sh/uv/install.sh | sh)"
	local -a template_ids=() template_origins=()
	if [[ "$OCI_STACK" == 1 ]]; then
		# Overlaybd only: the suite exercises the SDK contract on the on-demand
		# path (the server resolves the OCI template's status.manifestRef on
		# template-mode creates). OCI_STACK=0 falls back to the native template.
		local oci_template_id
		oci_template_id="$(cat "$WORK/overlaybd-template-id" 2>/dev/null || true)"
		[[ -n "$oci_template_id" ]] \
			|| die "no overlaybd template id at $WORK/overlaybd-template-id (run verify-overlaybd first; template_up_overlaybd writes it)"
		template_ids+=("$oci_template_id")
		template_origins+=("fast-sandbox-env-oci")
	else
		local template_id
		template_id="$(cat "$WORK/template-id" 2>/dev/null || true)"
		[[ -n "$template_id" ]] || die "no template id at $WORK/template-id (run up first; template_up stores it there)"
		template_ids+=("$template_id")
		template_origins+=("fast-sandbox-env")
	fi
	export OPENSANDBOX_TEST_DOMAIN="127.0.0.1:$SERVER_HOST_PORT"
	export OPENSANDBOX_TEST_PROTOCOL="http"
	export OPENSANDBOX_TEST_API_KEY="$SERVER_API_KEY"
	cd "$OSB_ROOT/tests/python" || die "tests/python not found under $OSB_ROOT"
	# No exec: the ERR trap (and the failure dump) must survive a pytest
	# failure, and exec replaces the shell. One pytest run per template so a
	# failure names its template.
	local index
	for index in "${!template_ids[@]}"; do
		export OPENSANDBOX_TEST_FSB_TEMPLATE_ID="${template_ids[$index]}"
		export OPENSANDBOX_TEST_FSB_TEMPLATE_ORIGIN="${template_origins[$index]}"
		log "sdk e2e: template=${template_ids[$index]} (origin=${template_origins[$index]}) server=$SERVER_URL -> tests/python/tests/test_fsb_e2e.py"
		uv run pytest tests/test_fsb_e2e.py
	done
}

idle_e2e() {
	if [[ "$IDLE_PAUSE" != "1" && ! -f "$WORK/idle-pause" ]]; then
		die "idle-e2e requires the idle-pause profile (IDLE_PAUSE=1 up)"
	fi
	kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
		|| die "cluster $KIND_CLUSTER is not up (run up first)"
	curl -fsS -m 5 "$SERVER_URL/health" >/dev/null 2>&1 \
		|| die "server $SERVER_URL is not reachable (run up first)"
	command -v uv >/dev/null 2>&1 \
		|| die "uv is required on PATH (https://docs.astral.sh/uv/ — curl -LsSf https://astral.sh/uv/install.sh | sh)"
	local template_id
	if [[ "$OCI_STACK" == 1 ]]; then
		# Overlaybd only (the on-demand path); OCI_STACK=0 falls back to native.
		template_id="$(cat "$WORK/overlaybd-template-id" 2>/dev/null || true)"
		[[ -n "$template_id" ]] \
			|| die "no overlaybd template id at $WORK/overlaybd-template-id (run verify-overlaybd first; template_up_overlaybd writes it)"
	else
		template_id="$(cat "$WORK/template-id" 2>/dev/null || true)"
		[[ -n "$template_id" ]] || die "no template id at $WORK/template-id (run up first; template_up stores it there)"
	fi
	export OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$template_id"
	export OPENSANDBOX_TEST_DOMAIN="127.0.0.1:$SERVER_HOST_PORT"
	export OPENSANDBOX_TEST_PROTOCOL="http"
	export OPENSANDBOX_TEST_API_KEY="$SERVER_API_KEY"
	export OPENSANDBOX_TEST_IDLE_PAUSE="1"
	export OPENSANDBOX_TEST_IDLE_ROUNDS="${IDLE_PAUSE_ROUNDS:-3}"
	log "sdk e2e (idle-pause): template=$template_id server=$SERVER_URL rounds=$OPENSANDBOX_TEST_IDLE_ROUNDS -> tests/python/tests/test_fsb_idle_pause_e2e.py"
	cd "$OSB_ROOT/tests/python" || die "tests/python not found under $OSB_ROOT"
	uv run pytest tests/test_fsb_idle_pause_e2e.py
}

# --- verify actions (native / overlaybd) --------------------------------------

require_live_stack() {
	kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
		|| die "cluster $KIND_CLUSTER is not up (run up first)"
	curl -fsS -m 5 "$SERVER_URL/health" >/dev/null 2>&1 \
		|| die "server $SERVER_URL is not reachable (run up first)"
	[[ -n "$OSB_BIN" ]] || OSB_BIN="$OSB_VENV/bin/osb"
	[[ -x "$OSB_BIN" ]] || die "osb not found at $OSB_BIN (run up first, or set OSB_BIN)"
}

verify_native() {
	require_live_stack
	TEMPLATE_ID="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$TEMPLATE_ID" ]] || die "no template id at $WORK/template-id (run up first; template_up stores it there)"
	run_stage "end-to-end verify (API create -> gateway -> execd /ping)" opensandbox_verify
	run_stage "pause/resume verify (CLI -> FastPath checkpoint)" pause_resume_verify
	run_stage "vault lifecycle verify (CLI -> relay -> egress-held vault)" vault_lifecycle_verify
	run_stage "MITM CA chain verify (baked CA -> injection -> pinning)" mitm_ca_chain_verify
	run_stage "idle-pause / wake-on-access verify (OSEP-0024 profile)" idle_pause_verify
	run_stage "snapshot verify (CLI -> SandboxSnapshot -> restore)" snapshot_verify
}

verify_overlaybd() {
	require_live_stack
	[[ "$OCI_STACK" == 1 ]] || {
		log "OCI_STACK=0: overlaybd disabled; nothing to verify"
		return 0
	}
	run_stage "SandboxTemplate build (overlaybd, oci:// publication)" template_up_overlaybd
	run_stage "end-to-end verify (overlaybd template)" opensandbox_verify "$OCI_TEMPLATE_ID"
	run_stage "pause/resume verify (overlaybd template)" pause_resume_verify "$OCI_TEMPLATE_ID"
	run_stage "vault lifecycle verify (overlaybd template)" vault_lifecycle_verify "$OCI_TEMPLATE_ID"
	run_stage "MITM CA chain verify (overlaybd template)" mitm_ca_chain_verify "$OCI_TEMPLATE_ID"
	run_stage "idle-pause / wake-on-access verify (overlaybd template)" idle_pause_verify "$OCI_TEMPLATE_ID"
	run_stage "snapshot verify (overlaybd template)" snapshot_verify "$OCI_TEMPLATE_ID"
}

# --- status / summary ---------------------------------------------------------------------

dart_metrics_summary() {
	local pods pod node metrics
	pods="$(runtime_pods 2>/dev/null || true)"
	[[ -n "$pods" ]] || { echo "  (no agent pods)"; return 0; }
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
		metrics="$(kubectl exec -n "$NS" "$pod" -- sh -c 'curl -fsS --noproxy "*" http://127.0.0.1:8147/metrics' 2>/dev/null || true)"
		echo "  $node:"
		if [[ -z "$metrics" ]]; then
			echo "    (DART metrics unreachable)"
			continue
		fi
		printf '%s\n' "$metrics" | grep -E '^dart_block_source_total\{source="(cache|peer|origin)"\}' \
			| sed 's/^/    /' || true
	done
}

status() {
	log "status: RustFS"
	docker ps --filter "name=$RUSTFS_CONTAINER" --format '{{.Names}} {{.Status}}' 2>/dev/null || true
	echo
	log "status: kind cluster / nodes"
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		kubectl get nodes -o wide
	else
		log "kind cluster $KIND_CLUSTER: down"
		return 0
	fi
	echo
	log "status: pods ($NS)"
	if [[ "$IDLE_PAUSE" == "1" ]]; then
		log "status: idle-pause Redis ($OSB_NS)"
		kubectl -n "$OSB_NS" get deploy,svc -l app=idle-pause-redis -o wide 2>/dev/null || true
		echo
	fi
	kubectl -n "$NS" get pods -o wide
	kubectl -n "$RESOURCE_NS" get pods -o wide
	echo
	log "status: SandboxPool"
	kubectl -n "$RESOURCE_NS" get sandboxpool \
		-o custom-columns='NAME:.metadata.name,RUNTIME:.spec.runtime,READY:.status.readyPods,CAPACITY:.spec.capacity.poolMin,WARM_IMAGES:.status.warmImages' 2>/dev/null || true
	echo
	log "status: fastlet pods (egress sidecar)"
	kubectl -n "$RESOURCE_NS" get pods -l app=sandbox-fastlet \
		-o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,CONTAINERS:.status.containerStatuses[*].name,READY:.status.containerStatuses[*].ready' 2>/dev/null || true
	echo
	log "status: DART P2P (block_source cache/peer/origin per node)"
	dart_metrics_summary || true
	echo
	log "status: OpenSandbox ($OSB_NS)"
	if kubectl get namespace "$OSB_NS" >/dev/null 2>&1; then
		kubectl -n "$OSB_NS" get pods -o wide
		printf '  server health:  %s\n' "$(curl -fsS -m 5 "$SERVER_URL/health" >/dev/null 2>&1 && echo OK || echo unreachable)"
		printf '  gateway health: %s\n' "$(curl -fsS -m 5 "$GATEWAY_URL/status.ok" >/dev/null 2>&1 && echo OK || echo unreachable)"
		printf '  server URL:     %s (header OPEN-SANDBOX-API-KEY: %s)\n' "$SERVER_URL" "$SERVER_API_KEY"
		printf '  gateway URL:    %s (header routing, signed f1.* scopes)\n' "$GATEWAY_URL"
	else
		echo "  (not deployed)"
	fi
	if [[ "$OCI_STACK" == 1 ]]; then
		log "status: OverlayBD OCI"
		if kubectl -n "$RESOURCE_NS" get deploy registry >/dev/null 2>&1; then
			printf '  registry:       %s (%s)\n' "$OCI_REGISTRY_HOST" \
				"$(kubectl -n "$RESOURCE_NS" get deploy registry -o jsonpath='{.status.readyReplicas}/{.status.replicas}')"
		else
			echo "  registry:       (not deployed)"
		fi
		local tid ref
		for tid in "$(cat "$WORK/template-id" 2>/dev/null || true)" "$(cat "$WORK/overlaybd-template-id" 2>/dev/null || true)"; do
			[[ -n "$tid" ]] || continue
			ref="$(osb_api template get "$tid" -o json 2>/dev/null | jq -r '[.status.phase, .status.manifest_ref // "-"] | join(" ")')"
			printf '  template %s: %s\n' "$tid" "${ref:-unreachable}"
		done
	fi
}

env_summary() {
	highlight "== environment summary =="
	printf '  %-22s %s\n' "kind cluster" "$KIND_CLUSTER ($(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ') nodes)"
	printf '  %-22s %s\n' "fast-sandbox" "pinned $(git -C "$FSB_DIR" rev-parse --short HEAD 2>/dev/null || echo "$FSB_COMMIT") ($FSB_DIR)"
	printf '  %-22s %s\n' "RustFS endpoint" "$RUSTFS_ENDPOINT"
	printf '  %-22s %s\n' "pool" "$POOL_NAME (runtime=firecracker, poolMin=$POOL_MIN, egress=$IMG_EGRESS)"
	# The echo endpoint line is conditional: the standalone pool action
	# loads the fingerprint pin but never re-derives the echo container IP.
	local mitm_echo_at=""
	[[ -n "$MITM_ECHO_IP" ]] && mitm_echo_at=", echo=https://$MITM_ECHO_DOMAIN via $MITM_ECHO_IP:$MITM_ECHO_PORT"
	printf '  %-22s %s\n' "MITM CA" "$MITM_CA_SECRET (fingerprint ${MITM_CA_FINGERPRINT:0:16}…${mitm_echo_at})"
	printf '  %-22s %s\n' "P2P" "DART daemons=$(printf '%s' "$(runtime_pods)" | wc -w | tr -d ' ') (on-demand pulls: cache -> peer -> origin)"
	printf '  %-22s %s\n' "template" "${TEMPLATE_ID:-n/a} ($(if [[ -n "$TEMPLATE_ID" ]]; then _template_phase || echo unknown; else echo "not built"; fi))"
	printf '  %-22s %s\n' "StateRoot fs" "$(findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null || echo 'plain directory (full copy per sandbox)')"
	printf '  %-22s %s\n' "server" "$IMG_SERVER -> $SERVER_URL (fsb runtime)"
	printf '  %-22s %s\n' "ingress gateway" "$IMG_INGRESS -> $GATEWAY_URL (fsb provider, header mode)"
	printf '  %-22s %s\n' "fastpath" "$FASTPATH_ENDPOINT"
	printf '  %-22s %s\n' "logs" "$LOGS_DIR"
}

# --- down ------------------------------------------------------------------------------------

down() {
	log "down: teardown"
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		kind delete cluster --name "$KIND_CLUSTER" > "$LOGS_DIR/kind-delete.log" 2>&1 || true
	fi
	[[ -z "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
		|| fail "kind cluster $KIND_CLUSTER still exists after delete"
	docker rm -f "$RUSTFS_CONTAINER" >/dev/null 2>&1 || true
	[[ -z "$(docker ps -a --filter "name=$RUSTFS_CONTAINER" --format '{{.Names}}' || true)" ]] \
		|| fail "RustFS container still present"
	# TLS echo endpoint + its CA material die with the run (regenerated fresh
	# on every up; a stale CA must never resurface).
	docker rm -f "$MITM_ECHO_CONTAINER" >/dev/null 2>&1 || true
	[[ -z "$(docker ps -a --filter "name=$MITM_ECHO_CONTAINER" --format '{{.Names}}' || true)" ]] \
		|| fail "MITM echo container still present"
	rm -rf "$MITM_CA_DIR"
	# Server/gateway die with the cluster; only the signing key outlives it.
	rm -f "$WORK/agent-registry.json" "$SIGNING_KEY_FILE"
	rm -rf "$GEN_DIR" "$FSB_GEN_DIR"
	# A stale profile marker would let idle-e2e run against a stack whose
	# Redis/sweeper are gone.
	rm -f "$WORK/idle-pause"
	# Same stale-marker risk: sdk-e2e and idle-e2e read
	# $WORK/overlaybd-template-id, and their producer (template_up_overlaybd)
	# only runs in verify-overlaybd.
	rm -f "$WORK/overlaybd-template-id"
	# RustFS data is owned by UID/GID 10001; leftovers pollute the host and
	# later docker build contexts.
	sudo_ rm -rf "$RUSTFS_DATA"
	sudo_ rm -rf "$RC_CONFIG_DIR"
	sysctl_restore
	stateroot_xfs_down
	# A committed DART cache is FINAL (never refreshed): purge it, or a
	# rebuilt SandboxTemplate stays ignored when the StateRoot survives.
	stateroot_scrub
	pass "host cleanup complete"
}

# --- main --------------------------------------------------------------------------------------

usage() {
	cat <<'EOF'
usage: integration-env.sh [--auto-clean] {up|verify-native|verify-overlaybd|down|status|pool|sdk-e2e|idle-e2e}

  up       initialize the environment: fast-sandbox@pinned-commit images,
           two-node kind cluster (KVM), RustFS, control plane, firecracker
           runtime readiness + DART (P2P), the native SandboxTemplate golden
           image, firecracker-egress-pool (egress attached), and the
           source-built OpenSandbox server + ingress gateway. With
           IDLE_PAUSE=1: Redis + server [automation.idle_pause] + gateway
           activity/wake flags.
  verify-native    run the verify matrix against the native template
                   (create -> gateway route -> execd /ping, pause/resume,
                   vault, idle-pause, snapshot); requires `up`
  verify-overlaybd build the overlaybd SandboxTemplate (oci:// publication)
                   and run the same verify matrix against it; requires `up`
                   (no-op when OCI_STACK=0)
  pool     re-apply only the SandboxPool (after editing manifests/pool/)
  status   nodes / pods / pool / DART P2P counters / RustFS / OpenSandbox health
  sdk-e2e  run the Python SDK e2e suite (tests/python/tests/test_fsb_e2e.py)
           against the live stack; requires `up` (template id at
           $WORK/template-id) and uv on PATH. Extra pytest args go through
           PYTEST_ADDOPTS.
  idle-e2e run the OSEP-0024 idle-pause/wake SDK suite
           (tests/python/tests/test_fsb_idle_pause_e2e.py); requires an
           `up` with IDLE_PAUSE=1 and uv on PATH.
  down     teardown: kind cluster + RustFS + sysctl + XFS StateRoot + caches

  --auto-clean  on up failure, run down automatically before dumping logs

  The RustFS migration replaces MINIO_* and MC_IMAGE overrides. Before
  upgrading an existing environment, run `down` with the previous script.

Notable env overrides: WORK, FSB_DIR, KIND_CLUSTER, KIND_SINGLE,
DOCKER_MIRROR, RUSTFS_*, RC_IMAGE, EGRESS_IMAGE, SERVER_IMAGE, INGRESS_IMAGE,
FSB_GOPROXY (default direct; set e.g. https://mirrors.aliyun.com/goproxy/,direct
when the host cannot reach module VCS hosts directly),
IMAGE_<COMPONENT>, POOL_MIN/POOL_MAX, WARM_IMAGES=1, SBX_IMAGE, EXECD,
XFS_STATEROOT=0, OSB_PACKAGE/OSB_BIN, SKIP_TOOL_INSTALL=1, SKIP_LEFTOVER_CLEAN=1,
IDLE_PAUSE=1 (OSEP-0024 idle-pause/wake profile; REDIS_IMAGE overrides the
Redis image), OCI_STACK=0 (disable the OverlayBD OCI registry + node stack;
default on — native and overlaybd paths run side by side in one cluster).
See the header of this script.
EOF
	exit 1
}

# Allow focused tests to load the stage functions without starting the environment.
[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0

for arg in "$@"; do
	case "$arg" in
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|pool|sdk-e2e|idle-e2e|verify-native|verify-overlaybd) ACTION="$arg" ;;
		*) usage ;;
	esac
done
[[ -n "$ACTION" ]] || usage

mkdir -p "$WORK" "$LOGS_DIR"

case "$ACTION" in
	up)
		exec > >(tee -a "$WORK/run.log") 2>&1
		log "=== fast-sandbox-env up ($(date -u +%FT%TZ)) ==="
		{
			echo "environment snapshot ($(date -u +%FT%TZ))"
			command -v kind >/dev/null && kind --version
			kubectl version --client 2>/dev/null | head -1
			go version
			docker --version
			echo "cluster=$KIND_CLUSTER single=$KIND_SINGLE rustfs=$RUSTFS_IMAGE port=$RUSTFS_PORT bucket=$RUSTFS_BUCKET"
			echo "sbxImage=$SBX_IMAGE execd=$EXECD warmImages=$WARM_IMAGES ociStack=$OCI_STACK registry=$([[ "$OCI_STACK" == 1 ]] && echo "$OCI_REGISTRY_HOST" || echo off)"
			echo "pool=$POOL_NAME poolMin=$POOL_MIN egress=$IMG_EGRESS"
			echo "server=$IMG_SERVER ingress=$IMG_INGRESS fastpath=$FASTPATH_ENDPOINT"
			echo "images: controller=$IMG_CONTROLLER runtime=$IMG_RUNTIME"
		} > "$LOGS_DIR/environment.txt" 2>&1 || true
		if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
			|| docker ps -a --format '{{.Names}}' | grep -qx "$RUSTFS_CONTAINER"; then
			if [[ "$SKIP_LEFTOVER_CLEAN" == 1 ]]; then
				log "leftover resources detected; aborting (SKIP_LEFTOVER_CLEAN=1). Run 'integration-env.sh down' first"
				exit 1
			fi
			log "leftover resources detected; cleaning and rebuilding"
			down
		fi
		trap 'on_error up' ERR
		run_stage "preflight + tooling" preflight
		run_stage "opensandbox CLI (osb, latest from PyPI)" ensure_osb
		run_stage "sysctl (fs.inotify)" sysctl_set
		run_stage "fast-sandbox checkout @ pinned commit" ensure_fsb
		run_stage "OCI registry CA + serving certificate" registry_ca_up
		run_stage "ublk registration check" ublk_module_up
		run_stage "build images (fast-sandbox + OpenSandbox)" build_images
		run_stage "XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "render kind config" render_kind_config
		run_stage "kind cluster (KVM passthrough + labels)" kind_up
		run_stage "RustFS + bucket" rustfs_up
		run_stage "RustFS endpoint (kind network)" resolve_rustfs_endpoint
		run_stage "OCI registry (in-cluster, TLS)" registry_up
		run_stage "CRDs + control plane" control_plane_up
		run_stage "credentials (publish/pull)" credentials_up
		run_stage "MITM CA material (OSEP-0022 trust anchor)" mitm_ca_up
		run_stage "firecracker runtime readiness + DART (P2P)" runtime_up
		run_stage "idle-pause Redis (OSEP-0024 profile)" redis_up
		run_stage "OpenSandbox server + ingress gateway" opensandbox_up
		run_stage "TLS echo endpoint (cert signed by the pinned CA)" mitm_echo_up
		run_stage "SandboxTemplate build (opensandbox CLI)" template_up
		run_stage "SandboxPool $POOL_NAME (egress + P2P)" pool_up
		trap - ERR
		stage_summary
		env_summary
		highlight "== up complete: server=$SERVER_URL (header OPEN-SANDBOX-API-KEY: $SERVER_API_KEY), gateway=$GATEWAY_URL =="
		highlight "== next: verify-native, verify-overlaybd, sdk-e2e, idle-e2e =="
		;;
	pool)
		exec > >(tee -a "$WORK/run.log") 2>&1
		kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
			|| die "cluster $KIND_CLUSTER is not up (run up first)"
		trap 'on_error pool' ERR
		run_stage "SandboxPool $POOL_NAME (egress + P2P)" pool_up
		trap - ERR
		env_summary
		;;
	status)
		status
		;;
	sdk-e2e)
		kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
			|| die "cluster $KIND_CLUSTER is not up (run up first)"
		trap 'on_error sdk-e2e' ERR
		sdk_e2e
		trap - ERR
		;;
	idle-e2e)
		kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
			|| die "cluster $KIND_CLUSTER is not up (run up first)"
		trap 'on_error idle-e2e' ERR
		idle_e2e
		trap - ERR
		;;
	verify-native)
		exec > >(tee -a "$WORK/run.log") 2>&1
		log "=== fast-sandbox-env verify-native ($(date -u +%FT%TZ)) ==="
		trap 'on_error verify-native' ERR
		verify_native
		trap - ERR
		stage_summary
		;;
	verify-overlaybd)
		exec > >(tee -a "$WORK/run.log") 2>&1
		log "=== fast-sandbox-env verify-overlaybd ($(date -u +%FT%TZ)) ==="
		trap 'on_error verify-overlaybd' ERR
		verify_overlaybd
		trap - ERR
		stage_summary
		;;
	down)
		down
		;;
esac
