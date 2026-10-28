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

# lib.sh — shared base for the fast-sandbox integration environment scripts
# (sourced, never executed): configuration variables, logging + stage
# machinery, the ERR/failure-dump/--auto-clean contract, tool installation,
# and the raw server / osb CLI helpers every verify script shares.
#
# Layout: integration.sh (entry/orchestration), env.sh (environment
# up/down/status/pool), verify/*.sh (one script per verify stage).
# Usage docs: integration.sh.

# Source-once guard: re-sourcing must not reset run state.
[[ -n "${_FSB_ENV_LIB_SOURCED:-}" ]] && return 0
_FSB_ENV_LIB_SOURCED=1

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
IMG_RUNTIME="${IMAGE_RUNTIME:-opensandbox/fsb-firecracker-runtime:dev}"
IMG_EGRESS="${EGRESS_IMAGE:-docker.io/opensandbox/egress:latest}"

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
	# Subshells inherit ERR: propagate the error instead of tearing down
	# main-process resources; ignore SIGPIPE (cancelled runners).
	trap - ERR
	[[ "$BASHPID" == "$ENV_PID" ]] || exit "$rc"
	trap '' PIPE
	# Dump before --auto-clean teardown destroys the evidence.
	failure_dump "$task" || true
	# down lives in env.sh; standalone verify scripts have no teardown to run.
	if [[ "$AUTO_CLEAN" == 1 ]] && declare -F down >/dev/null; then
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
		env | grep -E '^(KIND_(CLUSTER|SINGLE|RETAIN|NODE_IMAGE)=|FSB_DIR=|SBX_IMAGE=|IMG_[A-Z_]+=|EGRESS_IMAGE=|EXECD=|WORK=|POOL_(NAME|MIN|MAX)=|INGRESS_IMAGE=|WARM_IMAGES=|XFS_(STATEROOT|SIZE)=|RUSTFS_(IMAGE|PORT|CONSOLE_PORT|BUCKET|CONTAINER|DATA|ENDPOINT)=|RC_IMAGE=)' || true
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
		echo "--- builder pods + logs (tail) ---"
		kubectl get pods -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --tail=80 2>&1 || true
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
# The golden-image template id: template_up sets it and persists it at
# $WORK/template-id; the verify scripts resolve it through
# require_template_id when run standalone.
TEMPLATE_ID=""

verify_sandbox_gone() {
	! osb_api sandbox get "$VERIFY_ID" -o json >/dev/null 2>&1
}

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

# --- standalone-verify helpers ------------------------------------------------------

# Locate the osb CLI without installing it (verify scripts run against an
# already-provisioned stack; only env `up` installs).
resolve_osb() {
	if [[ -z "$OSB_BIN" ]]; then
		OSB_BIN="$OSB_VENV/bin/osb"
	fi
	[[ -x "$OSB_BIN" ]] \
		|| die "osb not found at $OSB_BIN (run 'integration.sh up' first, or set OSB_BIN)"
	log "osb version: $("$OSB_BIN" --version 2>/dev/null | tail -n1 || echo unknown)"
}

# Resolve the golden-image template id for standalone verify runs.
require_template_id() {
	[[ -n "${TEMPLATE_ID:-}" ]] || TEMPLATE_ID="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$TEMPLATE_ID" ]] \
		|| die "no template id at $WORK/template-id (run 'integration.sh up' first)"
}

# --- artifact-store hygiene ---------------------------------------------------------

# Prune stale artifact sets from the test S3 (RustFS). Every pause publishes
# an instance-private checkpoint set (multi-GiB: rootfs + vmstate + memory)
# and crashed runs leave stale template sets, so the artifact-heavy verifies
# prune before they start — while no sandbox exists that could still restore
# from one. The baseline golden template survives: its published set (the
# manifest_ref directory) and every image index that points INTO it are
# kept; everything else under the store prefix goes. Best-effort by design:
# a failed delete is logged and pruned-skip never fails a verify (hygiene,
# not a functional gate).
prune_artifact_store() {
	local manifest_ref protected_digest entry key seg mref removed=0 kept=0
	local -a stale_sets=() stale_indexes=()
	local -A seen=()
	[[ -n "${TEMPLATE_ID:-}" ]] || { log "artifact prune: no template id; skipping"; return 0; }
	manifest_ref="$(osb_api template get "$TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')"
	[[ "$manifest_ref" == "s3://$RUSTFS_BUCKET/publish/"* ]] \
		|| { log "artifact prune: template manifest_ref unavailable; skipping"; return 0; }
	protected_digest="${manifest_ref#s3://$RUSTFS_BUCKET/publish/}"
	protected_digest="${protected_digest%%/*}"
	log "artifact prune: protecting template set publish/$protected_digest/ (and the indexes pointing into it)"
	while IFS= read -r key; do
		key="${key#/}"; key="${key#chain/$RUSTFS_BUCKET/}"; key="${key#publish/}"
		[[ "$key" == *"/"* ]] || continue
		seg="${key%%/*}"
		if [[ "$seg" == "$protected_digest" ]]; then
			kept=$((kept + 1))
		elif [[ "$seg" == "index" ]]; then
			entry="${key#index/}"
			[[ -n "$entry" ]] || continue
			mref="$(rc cat "chain/$RUSTFS_BUCKET/publish/index/$entry" 2>/dev/null | jq -r '.manifestRef // empty' 2>/dev/null)"
			if [[ "$mref" == "s3://$RUSTFS_BUCKET/publish/$protected_digest/"* ]]; then
				kept=$((kept + 1))
			elif [[ -z "${seen[$entry]:-}" ]]; then
				seen[$entry]=1
				stale_indexes+=("$entry")
			fi
		elif [[ -z "${seen[$seg]:-}" ]]; then
			seen[$seg]=1
			stale_sets+=("$seg")
		fi
	done < <(rc ls --recursive "chain/$RUSTFS_BUCKET/publish/" 2>/dev/null | awk '{print $NF}')
	for seg in "${stale_sets[@]:-}"; do
		[[ -n "$seg" ]] || continue
		if rc rm --recursive --force "chain/$RUSTFS_BUCKET/publish/$seg" >/dev/null 2>&1; then
			removed=$((removed + 1))
		else
			log "artifact prune: failed to remove publish/$seg/ (continuing)"
		fi
	done
	for entry in "${stale_indexes[@]:-}"; do
		[[ -n "$entry" ]] || continue
		if rc rm --force "chain/$RUSTFS_BUCKET/publish/index/$entry" >/dev/null 2>&1; then
			removed=$((removed + 1))
		else
			log "artifact prune: failed to remove publish/index/$entry (continuing)"
		fi
	done
	log "artifact prune: removed ${#stale_sets[@]} stale set(s) + ${#stale_indexes[@]} stale index(es) ($removed delete call(s), baseline kept: $kept object(s))"
}

# Workspace directories must exist before any log()/stage call.
mkdir -p "$WORK" "$LOGS_DIR" "$GEN_DIR"
