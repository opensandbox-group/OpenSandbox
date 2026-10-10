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
# egress sidecar — then verifies end to end: sandbox create via the
# opensandbox CLI, execd /ping through the signed gateway route, delete,
# a pause/resume round trip, and a snapshot -> restore-from-snapshotId boot.
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
#   ./scripts/fast-sandbox-env/integration-env.sh up       # full environment + pool + server/ingress + verify
#   ./scripts/fast-sandbox-env/integration-env.sh pool     # re-apply the pool only
#   ./scripts/fast-sandbox-env/integration-env.sh status   # component/pool/DART/OpenSandbox health
#   ./scripts/fast-sandbox-env/integration-env.sh down     # teardown, host left clean
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
fail() { printf '\033[1;31m[fast-sandbox-env] FAIL\033[0m %s\n' "$*" >&2; exit 1; }
highlight() { printf '\033[1;36m%s\033[0m\n' "$*"; }

now_ms() { date +%s%N; }   # GNU date (Linux); the script targets a Linux KVM host
ms2s() { awk -v ms="$1" 'BEGIN { printf "%.1f", ms / 1000 }'; }

declare -a STAGE_ORDER=()
declare -a STAGE_MS_LIST=()
STAGE_CUR=""
STAGE_START_NS=0
STAGE_N=0

stage_begin() {
	STAGE_N=$((STAGE_N + 1))
	STAGE_CUR="$1"
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
		echo "--- fastlet logs (tail) ---"
		kubectl logs -n "$RESOURCE_NS" -l app=sandbox-fastlet --tail=80 2>&1 || true
		echo "--- builder pods + logs (tail) ---"
		kubectl get pods -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$RESOURCE_NS" -l sandbox.fast.io/sandboxtemplate --tail=80 2>&1 || true
		echo "--- SandboxTemplates (status carries the build failure reason) ---"
		kubectl get sandboxtemplates -n "$RESOURCE_NS" -o yaml 2>&1 || true
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
		log "disk headroom: $target has $((avail_kb / 1024 / 1024 / 1024))G free"
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
	kubectl -n "$RESOURCE_NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=registries:
  - host: $host
    secretRef:
      name: registry-rustfs
" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written (publish/pull)"
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
		--set artifactStore.endpoint="$RUSTFS_ENDPOINT"
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
		wait_for "node $node labeled + FirecrackerReady" 120 \
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
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet -t "$IMG_BUILDER" \
		-f "$FSB_DIR/build/Dockerfile.sandboxtemplate-builder" "$FSB_DIR" >/dev/null \
		|| die "sandboxtemplate-builder image build failed"
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
	pass "SandboxTemplate Succeeded + artifacts published (via opensandbox CLI)"
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

render_pool() { # > $GEN_DIR/firecracker-egress-pool.yaml
	local src="$MANIFESTS_DIR/pool/firecracker-egress-pool.yaml" out="$GEN_DIR/firecracker-egress-pool.yaml"
	mkdir -p "$GEN_DIR"
	# Quoted tokens in the template keep rendered scalars' natural types
	# (poolMin stays an integer); awk for sed portability.
	awk -v fastlet="$IMG_FASTLET" -v egress="$IMG_EGRESS" \
		-v pool_min="$POOL_MIN" -v pool_max="$POOL_MAX" \
		-v max_per_pod="$MAX_SANDBOXES_PER_POD" \
		-v warm="$WARM_IMAGES" -v image="$TEMPLATE_ID" '
		{ gsub(/"@FASTLET_IMAGE@"/, fastlet)
		  gsub(/"@EGRESS_IMAGE@"/, egress)
		  gsub(/"@POOL_MIN@"/, pool_min)
		  gsub(/"@POOL_MAX@"/, pool_max)
		  gsub(/maxSandboxesPerPod: 5/, "maxSandboxesPerPod: " max_per_pod) }
		/^# @WARM_IMAGES@$/ {
			if (warm == "1") printf "  warmImages:\n  - %s\n", image
			next }
		{ print }
	' "$src" > "$out"
	# Leftover-token guard: match only value positions (key: ...@T@...),
	# never the header comments that document the tokens themselves.
	if grep -Eq '^[[:space:]]*[A-Za-z][A-Za-z0-9]*:.*@[A-Z_]+@' "$out"; then
		die "unrendered token left in $out"
	fi
}

pool_up() {
	render_pool
	log "applying SandboxPool $POOL_NAME (runtime=firecracker, egress attached, poolMin=$POOL_MIN)"
	kubectl apply -f "$GEN_DIR/firecracker-egress-pool.yaml" >/dev/null
	wait_for "fastlet pods Ready (poolMin=$POOL_MIN)" 300 fastlet_pods_ready
	wait_for "egress container ready in every fastlet pod" 180 egress_containers_ready
	wait_for "pool condition RuntimeReady=True" 120 pool_condition_true RuntimeReady
	wait_for "pool condition InfraReady=True" 120 pool_condition_true InfraReady
	wait_for "egress actions endpoint answering (/_fastlet/v1/actions/status)" 60 egress_status_ready
	if [[ "$WARM_IMAGES" == "1" ]]; then
		wait_for "pool warmImages Ready" 300 warm_images_ready
		p2p_evidence "warm preheat"
		pass "fastlet Running + warmImages Ready (P2P evidence captured)"
	else
		pass "fastlet Running, on-demand pull (default: first sandbox pulls through DART)"
	fi
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
		-v execd="$EXECD" '
		{ gsub(/@SERVER_API_KEY@/, api_key)
		  gsub(/@SIGNING_KEY@/, signing_key)
		  gsub(/@FASTPATH_ENDPOINT@/, fastpath)
		  gsub(/@FSB_NAMESPACE@/, fsb_ns)
		  gsub(/@POOL_NAME@/, pool)
		  gsub(/@EXECD_IMAGE@/, execd)
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
TOML
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
		--set gateway.resources.limits.memory=1Gi
	kubectl apply -f "$GEN_DIR/osb-ingress-gateway.yaml" >/dev/null
	wait_for "server deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-server --timeout=10s
	wait_for "ingress gateway deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-ingress-gateway --timeout=10s
	# A ready gateway proves FastPath reachability: it fails startup without
	# the gRPC connection.
	wait_for "server /health on 127.0.0.1:$SERVER_HOST_PORT" 60 \
		curl -fsS -m 5 "$SERVER_URL/health"
	wait_for "gateway /status.ok on 127.0.0.1:$GATEWAY_HOST_PORT" 60 \
		curl -fsS -m 5 "$GATEWAY_URL/status.ok"
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

# Full wire-up: CLI create (fsb) -> FastPath -> fastlet -> firecracker
# sandbox -> signed gateway route -> fastlet-proxy -> guest execd /ping ->
# delete. One sandbox per call; availability is measured from the client
# (poll /ping until 200, wall-clock budget — each route poll is a CLI call,
# ~0.5s startup), not from CR status.
verify_one_sandbox() { # <label> <ping-budget-ms>
	local label="$1" budget="$2" created t0 t1 t2 route code deadline
	local policy_file="$GEN_DIR/verify-policy.json"
	t0="$(now_ms)"
	# Egress policy rides on every create (binding -> nft rules), so the
	# chain is exercised each time; /ping is inbound and unaffected.
	jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "example.com"}, {action: "allow", target: "*.opensandbox.ai"}]}' \
		> "$policy_file"
	log "verify ($label): creating a sandbox via the opensandbox CLI (templateId=$TEMPLATE_ID)"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
		--metadata origin=fast-sandbox-env-verify \
		--network-policy-file "$policy_file" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL: $(osb_api sandbox create \
			--template "$TEMPLATE_ID" --timeout 1h --skip-health-check 2>&1 >/dev/null | head -c 400)"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	t1="$(now_ms)"
	# Availability = /ping 200 through the signed gateway route (port 44772).
	deadline=$(( t1 + budget * 1000000 ))
	while :; do
		route="$(osb_api sandbox endpoint "$VERIFY_ID" --port 44772 -o json 2>/dev/null \
			| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
		if [[ -n "$route" ]]; then
			code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' \
				-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" 2>/dev/null || true)"
			[[ "$code" == "200" ]] && break
		fi
		if (( $(now_ms) >= deadline )); then
			fail "execd /ping did not return 200 within ${budget}ms (last code=${code:-none}, route=${route:-none})"
		fi
		sleep 0.2
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
	out="$(osb_api sandbox get "$id" -o json 2>/dev/null)"
	[[ "$(printf '%s' "$out" | jq -r '.expires_at')" == "$renewed_expires" ]] \
		|| fail "renewed expiresAt not visible: expected $renewed_expires got $(printf '%s' "$out" | jq -r '.expires_at')"
	pass "lifecycle: osb sandbox renew 2h ($expected_expires -> $renewed_expires)"
}

opensandbox_verify() {
	# Cold create pulls the golden image through DART (slowest; generous
	# budget). Warm creates: the second may still pull on the other node via
	# its DART peer; after both nodes cache the set, creates are sub-second.
	local ids=() id label index
	verify_one_sandbox "cold #1" 600000
	ids+=("$VERIFY_ID")
	verify_one_sandbox "warm #2" 600000
	ids+=("$VERIFY_ID")
	for index in 3 4 5 6; do
		verify_one_sandbox "warm #$index" 120000
		ids+=("$VERIFY_ID")
	done
	pass "end-to-end: opensandbox CLI -> server -> FastPath -> fastlet -> sandbox execd -> gateway route OK (6 sandboxes)"
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

pause_resume_verify() {
	# Accepted + poll: Paused == checkpoint durable + capacity released;
	# resume advances the route generation, so /ping needs a fresh route.
	local created t0 t1 t2 t3
	log "verify (pause): creating a sandbox via the opensandbox CLI (templateId=$TEMPLATE_ID)"
	t0="$(now_ms)"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
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
	local created out source_id snapshot_id snapshot_id2 restore_id reentry_out reentry_code
	local t0 t1 t2 t3 t4
	t0="$(now_ms)"
	log "verify (snapshot): creating the source sandbox via the opensandbox CLI (templateId=$TEMPLATE_ID)"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
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
	local template_id
	template_id="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$template_id" ]] || die "no template id at $WORK/template-id (run up first; template_up stores it there)"
	export OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$template_id"
	export OPENSANDBOX_TEST_DOMAIN="127.0.0.1:$SERVER_HOST_PORT"
	export OPENSANDBOX_TEST_PROTOCOL="http"
	export OPENSANDBOX_TEST_API_KEY="$SERVER_API_KEY"
	log "sdk e2e: template=$template_id server=$SERVER_URL -> tests/python/tests/test_fsb_e2e.py"
	cd "$OSB_ROOT/tests/python" || die "tests/python not found under $OSB_ROOT"
	# No exec: the ERR trap (and the failure dump) must survive a pytest
	# failure, and exec replaces the shell.
	uv run pytest tests/test_fsb_e2e.py
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
}

env_summary() {
	highlight "== environment summary =="
	printf '  %-22s %s\n' "kind cluster" "$KIND_CLUSTER ($(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ') nodes)"
	printf '  %-22s %s\n' "fast-sandbox" "pinned $(git -C "$FSB_DIR" rev-parse --short HEAD 2>/dev/null || echo "$FSB_COMMIT") ($FSB_DIR)"
	printf '  %-22s %s\n' "RustFS endpoint" "$RUSTFS_ENDPOINT"
	printf '  %-22s %s\n' "pool" "$POOL_NAME (runtime=firecracker, poolMin=$POOL_MIN, egress=$IMG_EGRESS)"
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
	# Server/gateway die with the cluster; only the signing key outlives it.
	rm -f "$WORK/agent-registry.json" "$SIGNING_KEY_FILE"
	rm -rf "$GEN_DIR" "$FSB_GEN_DIR"
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
usage: integration-env.sh [--auto-clean] {up|down|status|pool|sdk-e2e}

  up       initialize the full environment: fast-sandbox@pinned-commit images,
           two-node kind cluster (KVM), RustFS, control plane, firecracker
           firecracker runtime readiness + DART (P2P), SandboxTemplate golden
           image, firecracker-egress-pool (egress attached), the
           source-built OpenSandbox server + ingress gateway, and
           end-to-end verifies driven by the opensandbox CLI
           (create -> gateway route -> execd /ping, plus a pause/resume
           round-trip through the checkpoint). The CLI (osb) is installed
           fresh from PyPI unless OSB_BIN points at an existing binary.
  pool     re-apply only the SandboxPool (after editing manifests/pool/)
  status   nodes / pods / pool / DART P2P counters / RustFS / OpenSandbox health
  sdk-e2e  run the Python SDK e2e suite (tests/python/tests/test_fsb_e2e.py)
           against the live stack; requires `up` (template id at
           $WORK/template-id) and uv on PATH. Extra pytest args go through
           PYTEST_ADDOPTS.
  down     teardown: kind cluster + RustFS + sysctl + XFS StateRoot + caches

  --auto-clean  on up failure, run down automatically before dumping logs

  The RustFS migration replaces MINIO_* and MC_IMAGE overrides. Before
  upgrading an existing environment, run `down` with the previous script.

Notable env overrides: WORK, FSB_DIR, KIND_CLUSTER, KIND_SINGLE,
DOCKER_MIRROR, RUSTFS_*, RC_IMAGE, EGRESS_IMAGE, SERVER_IMAGE, INGRESS_IMAGE,
FSB_GOPROXY (default direct; set e.g. https://mirrors.aliyun.com/goproxy/,direct
when the host cannot reach module VCS hosts directly),
IMAGE_<COMPONENT>, POOL_MIN/POOL_MAX, WARM_IMAGES=1, SBX_IMAGE, EXECD,
XFS_STATEROOT=0, OSB_PACKAGE/OSB_BIN, SKIP_TOOL_INSTALL=1, SKIP_LEFTOVER_CLEAN=1.
See the header of this script.
EOF
	exit 1
}

# Allow focused tests to load the stage functions without starting the environment.
[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0

for arg in "$@"; do
	case "$arg" in
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|pool|sdk-e2e) ACTION="$arg" ;;
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
			echo "sbxImage=$SBX_IMAGE execd=$EXECD warmImages=$WARM_IMAGES"
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
		run_stage "build images (fast-sandbox + OpenSandbox)" build_images
		run_stage "XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "render kind config" render_kind_config
		run_stage "kind cluster (KVM passthrough + labels)" kind_up
		run_stage "RustFS + bucket" rustfs_up
		run_stage "RustFS endpoint (kind network)" resolve_rustfs_endpoint
		run_stage "CRDs + control plane" control_plane_up
		run_stage "credentials (publish/pull)" credentials_up
		run_stage "firecracker runtime readiness + DART (P2P)" runtime_up
		run_stage "OpenSandbox server + ingress gateway" opensandbox_up
		run_stage "SandboxTemplate build (opensandbox CLI)" template_up
		run_stage "SandboxPool $POOL_NAME (egress + P2P)" pool_up
		run_stage "end-to-end verify (CLI create -> gateway -> execd /ping)" opensandbox_verify
		run_stage "pause/resume verify (CLI -> FastPath checkpoint)" pause_resume_verify
		run_stage "snapshot verify (CLI -> SandboxSnapshot -> restore)" snapshot_verify
		trap - ERR
		stage_summary
		env_summary
		highlight "== up complete: server=$SERVER_URL (header OPEN-SANDBOX-API-KEY: $SERVER_API_KEY), gateway=$GATEWAY_URL =="
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
	down)
		down
		;;
esac
