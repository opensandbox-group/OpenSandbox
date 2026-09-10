#!/usr/bin/env bash
# Copyright 2026 Alibaba Group Holding Ltd.
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

# fast-sandbox-env.sh — one-command fast-sandbox integration environment,
# driven from the OpenSandbox repository.
#
# Builds the full OpenSandbox ecosystem on a bare-metal Linux KVM host
# (fast-sandbox checked out at master): two-node kind cluster with KVM
# passthrough → CRDs + all-in-one control plane → MinIO artifact store →
# node runtime installers → runtime-agent + node-local DART daemons (P2P
# is the default data plane) → the firecracker-egress-pool SandboxPool
# with the OpenSandbox egress sidecar attached through the Sandbox Actions
# channel → the source-built OpenSandbox lifecycle server (fsb runtime)
# and ingress gateway → an end-to-end verify (create through the server
# API, execd /ping through the signed gateway route, delete).
#
# Everything is source-built from this repository plus fast-sandbox@master,
# except the execd image baked into the SandboxTemplate golden image
# (EXECD, published image by default).
#
# Usage:
#   ./scripts/fast-sandbox-env/fast-sandbox-env.sh up       # full environment + pool + server/ingress + verify
#   ./scripts/fast-sandbox-env/fast-sandbox-env.sh pool     # re-apply the pool only
#   ./scripts/fast-sandbox-env/fast-sandbox-env.sh status   # component/pool/DART/OpenSandbox health
#   ./scripts/fast-sandbox-env/fast-sandbox-env.sh down     # teardown, host left clean
#   ./scripts/fast-sandbox-env/fast-sandbox-env.sh up --auto-clean   # down on failure
#
# Environment overrides (all optional):
#   WORK                 workspace + logs        (default $PWD/.fast-sandbox-env)
#   FSB_DIR              fast-sandbox checkout  (default $WORK/fast-sandbox —
#                        env-owned clone, created from FSB_GIT_URL when missing)
#   FSB_GIT_URL / FSB_REF                       (default opensandbox-group/fast-sandbox, master)
#   KIND_CLUSTER / KIND_NODE_IMAGE / KIND_RETAIN / KIND_SINGLE
#   DOCKER_MIRROR        comma list injected as docker.io containerd mirrors
#   MINIO_PORT / MINIO_CONSOLE_PORT / MINIO_AK / MINIO_SK / MINIO_IMAGE / MINIO_ENDPOINT
#   IMAGE_<NAME>         fast-sandbox component image tags
#   EGRESS_IMAGE         egress image tag        (default docker.io/opensandbox/egress:latest)
#   SERVER_IMAGE / INGRESS_IMAGE  OpenSandbox server/ingress image tags
#   SERVER_HOST_PORT / GATEWAY_HOST_PORT  host-side publishes (default 8080/8081)
#   WARM_IMAGES=1        preheat pool warmImages (default: on-demand first-sandbox pull)
#   SBX_IMAGE / EXECD    template build inputs   (default alpine:3.19 / opensandbox/execd:1.1.0)
#   POOL_MIN / POOL_MAX  pool capacity           (default 2/2; auto 1/1 when KIND_SINGLE=1)
#   XFS_STATEROOT / XFS_SIZE  reflink StateRoot on/off and virtual size
#   SKIP_TOOL_INSTALL / SKIP_LEFTOVER_CLEAN / INOTIFY_VALUE
#
# Every stage logs to $WORK/logs/; failures dump component logs to
# logs/failure-<task>-<ts>.txt before exiting (never silently).

set -euo pipefail

OSB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="$SCRIPT_DIR/manifests"
WORK="${WORK:-$PWD/.fast-sandbox-env}"
LOGS_DIR="$WORK/logs"
GEN_DIR="$WORK/gen"

# Env-owned fast-sandbox clone under $WORK (independent of any checkout
# outside the workspace); FSB_DIR still overrides for an existing one.
FSB_DIR="${FSB_DIR:-$WORK/fast-sandbox}"
FSB_GIT_URL="${FSB_GIT_URL:-https://github.com/opensandbox-group/fast-sandbox.git}"
FSB_REF="${FSB_REF:-master}"

KIND_CLUSTER="${KIND_CLUSTER:-fast-sandbox-integration}"
KIND_SINGLE="${KIND_SINGLE:-0}"
KIND_RETAIN="${KIND_RETAIN:-0}"
NS="fast-sandbox-system"

MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:latest}"
MINIO_PORT="${MINIO_PORT:-9000}"
# The container always LISTENS on 9000 (guest side of the publish map and
# the port kind-network clients use via the container IP); MINIO_PORT only
# moves the host-side 127.0.0.1 publish.
MINIO_CONTAINER_PORT=9000
# Console (human-only UI) listens on 9001 in-container; the host-side
# publish is overridable because 9001 is a common host-port collision.
MINIO_CONSOLE_PORT="${MINIO_CONSOLE_PORT:-9001}"
MINIO_AK="${MINIO_AK:-integration-env}"
MINIO_SK="${MINIO_SK:-integration-env-secret}"
MINIO_BUCKET="sandbox-images"
MINIO_CONTAINER="${MINIO_CONTAINER:-fast-sandbox-env-minio}"
MINIO_DATA="$WORK/minio-data"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-}"   # auto-derived from the kind network

SBX_IMAGE="${SBX_IMAGE:-alpine:3.19}"
EXECD="${EXECD:-opensandbox/execd:1.1.0}"
# WARM_IMAGES=1 preheats the pool instead of the default on-demand flow
# (warmImages reference the template id: the exact per-template index key).
# (first sandbox create on each node pulls the artifact set through DART).
WARM_IMAGES="${WARM_IMAGES:-0}"

POOL_NAME="${POOL_NAME:-firecracker-egress-pool}"
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
IMG_CONTROLLER="${IMAGE_CONTROLLER:-fast-sandbox/controller:dev}"
IMG_FASTLET="${IMAGE_FASTLET:-fast-sandbox/fastlet:dev}"
IMG_FASTLET_PROXY="${IMAGE_FASTLET_PROXY:-fast-sandbox/fastlet-proxy:dev}"
IMG_SANDBOX_PROXY="${IMAGE_SANDBOX_PROXY:-fast-sandbox/sandbox-proxy:dev}"
IMG_JANITOR="${IMAGE_JANITOR:-fast-sandbox/janitor:dev}"
IMG_BUILDER="${IMAGE_BUILDER:-fast-sandbox/sandboxtemplate-builder:dev}"
IMG_AGENT="${IMAGE_AGENT:-fast-sandbox/firecracker-runtime-agent:dev}"
IMG_EGRESS="${EGRESS_IMAGE:-docker.io/opensandbox/egress:latest}"

# --- OpenSandbox server + ingress gateway (source-built) ----------------------
# Fixed shape of this environment: no knobs, the full stack always runs.

OSB_NS="opensandbox-system"
IMG_SERVER="${SERVER_IMAGE:-docker.io/opensandbox/server:env}"
IMG_INGRESS="${INGRESS_IMAGE:-docker.io/opensandbox/ingress:env}"
# FastPath v2 of this cluster's all-in-one control plane (in-cluster DNS).
FASTPATH_ENDPOINT="fast-sandbox-fastpath.fast-sandbox-system.svc:9090"
SERVER_API_KEY="fast-sandbox-env"
# Shared f1.* route-scope signing key (server [ingress.secure_access] and
# ingress --secure-access-keys), generated once per workdir so re-applies
# keep previously issued routes verifiable.
SIGNING_KEY_FILE="$WORK/opensandbox-signing-key"
# Host-side publish (kind extraPortMappings on the control-plane node, bound
# to 127.0.0.1 only): Service NodePorts -> server :80 / gateway :28888.
# Overridable because host port collisions are environment-specific.
SERVER_HOST_PORT="${SERVER_HOST_PORT:-8080}"
GATEWAY_HOST_PORT="${GATEWAY_HOST_PORT:-8081}"
SERVER_NODEPORT=30880
GATEWAY_NODEPORT=30881
GATEWAY_ADDRESS="127.0.0.1:$GATEWAY_HOST_PORT"
SERVER_URL="http://127.0.0.1:$SERVER_HOST_PORT"
GATEWAY_URL="http://127.0.0.1:$GATEWAY_HOST_PORT"

# Node labels: sandbox.fast.io/kvm is hardcoded by the SandboxTemplate
# reconciler; fast-sandbox.io/firecracker-node selects installer/agent/fastlet.
KVM_NODE_LABEL="sandbox.fast.io/kvm"
FC_NODE_LABEL="fast-sandbox.io/firecracker-node"

INOTIFY_VALUE="${INOTIFY_VALUE:-8192}"
SYSCTL_BACKUP="$WORK/sysctl-backup"
SKIP_TOOL_INSTALL="${SKIP_TOOL_INSTALL:-0}"
SKIP_LEFTOVER_CLEAN="${SKIP_LEFTOVER_CLEAN:-0}"
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.0}"

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

kubectl_get() { kubectl -n "$NS" get "$1" -o jsonpath="$2"; }

sudo_() { if [[ "$(id -u)" == 0 ]]; then "$@"; else sudo "$@"; fi; }

# --- helpers ---------------------------------------------------------------------

kind_node() { kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1; }

kind_network() { # docker network of the first node container
	local node
	node="$(kind_node)" || return 1
	docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$node" | tr ' ' '\n' | grep -v '^$' | head -1
}

mc() { docker run --rm --network host -v "$WORK/mc-config:/root/.mc" minio/mc "$@"; }

# --- failure dump ------------------------------------------------------------------

on_error() {
	local task="$1"
	if [[ "$AUTO_CLEAN" == 1 ]]; then
		log "$ACTION failed at $task; --auto-clean: running down"
		down >/dev/null 2>&1 || true
	fi
	failure_dump "$task"
	printf '\033[1;31m[fast-sandbox-env] FAILED at %s; dump: %s\033[0m\n' \
		"$task" "$LOGS_DIR/failure-$task-*.txt" >&2
}

failure_dump() {
	local task="$1"
	local dump="$LOGS_DIR/failure-$task-$(date +%s).txt"
	mkdir -p "$LOGS_DIR"
	{
		echo "=== fast-sandbox-env failure: $task ($(date -u +%FT%TZ)) ==="
		env | grep -E '^(MINIO|KIND|FSB_|SBX|IMG_|EGRESS|EXECD|WORK|POOL|SERVER|INGRESS|WARM|XFS)' || true
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
		echo "--- agent logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-agent --tail=80 2>&1 || true
		echo "--- installer logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-installer --all-containers --tail=80 2>&1 || true
		echo "--- fastlet logs (tail) ---"
		kubectl logs -n "$NS" -l app=sandbox-fastlet --tail=80 2>&1 || true
		echo "--- builder pods + logs (tail) ---"
		kubectl get pods -n "$NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$NS" -l sandbox.fast.io/sandboxtemplate --tail=80 2>&1 || true
		echo "--- SandboxTemplates (status carries the build failure reason) ---"
		kubectl get sandboxtemplates -n "$NS" -o yaml 2>&1 || true
		echo "--- recent events ($NS) ---"
		kubectl get events -n "$NS" --sort-by=.lastTimestamp 2>&1 | tail -30 || true
		echo "--- OpenSandbox pods ($OSB_NS) ---"
		kubectl get pods -n "$OSB_NS" -o wide 2>&1 || true
		echo "--- server logs (tail) ---"
		kubectl logs -n "$OSB_NS" deploy/opensandbox-server --tail=80 2>&1 || true
		echo "--- ingress gateway logs (tail) ---"
		kubectl logs -n "$OSB_NS" deploy/opensandbox-ingress-gateway --tail=80 2>&1 || true
		echo "--- pool ---"
		kubectl get sandboxpool -n "$NS" -o yaml 2>&1 || true
		echo "--- minio docker logs (tail) ---"
		docker logs "$MINIO_CONTAINER" --tail=80 2>&1 || true
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

host_port_busy() { # port -> 0 when something already listens on 127.0.0.1:<port>
	(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

preflight() {
	[[ "$(uname -s)" == "Linux" ]] \
		|| die "this environment requires a Linux host with KVM (run it on the remote development VM)"
	command -v docker >/dev/null || die "docker is required"
	command -v go >/dev/null || die "go is required (>=1.25, used by make images and gen-registry)"
	ensure_tool kind
	ensure_tool kubectl
	ensure_tool jq
	docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
	local cgver
	cgver="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || true)"
	log "docker cgroup version=$cgver"
	# kind requires cgroup v2: on v1 hosts kubelet fails to create the
	# kubepods cgroup regardless of the docker cgroup driver.
	if [[ "$cgver" == "1" ]]; then
		die "docker cgroup Version is 1; kind requires cgroup v2. Enable it with the kernel cmdline 'systemd.unified_cgroup_hierarchy=1' and reboot"
	fi
	[[ -e /dev/kvm ]] || die "/dev/kvm is missing on this host (KVM required)"
	# The workspace filesystem carries docker images (built), the XFS
	# StateRoot loop file (sparse, up to 24G), MinIO artifacts (~4G per
	# template build) and node snapshot caches; MinIO refuses writes
	# below its free-disk threshold, so guard it here with a clear error.
	local min_free_kb=$((30 * 1024 * 1024)) free_kb
	free_kb="$(df -Pk "$WORK" 2>/dev/null | awk 'NR==2 {print $4}')"
	[[ "$free_kb" =~ ^[0-9]+$ ]] || die "cannot determine free disk space on $WORK"
	if (( free_kb < min_free_kb )); then
		die "only $((free_kb / 1024 / 1024))G free on $WORK (need 30G: images + XFS StateRoot + MinIO artifacts); free space (docker system prune / old kind clusters) or point WORK at a bigger volume"
	fi
	# Fail fast on busy host ports instead of dying at the docker bind or
	# kind create. MinIO culprits: a leftover MinIO container; 8080/8081 are
	# published by the kind node for the server / ingress gateway.
	local port
	for port in "$MINIO_PORT" "$MINIO_CONSOLE_PORT" "$SERVER_HOST_PORT" "$GATEWAY_HOST_PORT"; do
		if host_port_busy "$port"; then
			die "127.0.0.1:$port is already in use (check 'ss -ltnp' / 'docker ps'); free it, or set MINIO_PORT / MINIO_CONSOLE_PORT / SERVER_HOST_PORT / GATEWAY_HOST_PORT"
		fi
	done
	docker pull -q "$MINIO_IMAGE" >/dev/null
	docker pull -q minio/mc >/dev/null
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

# Scratch Go sources compiled inside the fast-sandbox module (gen-registry
# imports internal/registryconfig). The directory lives inside the checkout
# so `go run` resolves the module; it is removed before every dirty check
# and on down.
FSB_GEN_DIR="$FSB_DIR/.fast-sandbox-env-gen"

ensure_fsb() {
	if [[ ! -d "$FSB_DIR/.git" ]]; then
		log "cloning fast-sandbox ($FSB_GIT_URL) into $FSB_DIR"
		git clone "$FSB_GIT_URL" "$FSB_DIR" || die "clone failed; check FSB_GIT_URL / network"
	fi
	# An existing checkout keeps its original origin; repoint it when
	# FSB_GIT_URL targets a different source (e.g. a fork branch), so the
	# fetch/ff-merge below run against the intended remote.
	local current_url
	current_url="$(git -C "$FSB_DIR" remote get-url origin)"
	if [[ "$current_url" != "$FSB_GIT_URL" ]]; then
		log "fast-sandbox origin: $current_url -> $FSB_GIT_URL"
		git -C "$FSB_DIR" remote set-url origin "$FSB_GIT_URL" \
			|| die "could not repoint fast-sandbox origin at $FSB_GIT_URL"
	fi
	rm -rf "$FSB_GEN_DIR"
	[[ -z "$(git -C "$FSB_DIR" status --porcelain)" ]] \
		|| die "fast-sandbox checkout at $FSB_DIR has local changes; delete it to re-clone or point FSB_DIR at a clean checkout"
	git -C "$FSB_DIR" fetch -q origin "$FSB_REF" || die "git fetch origin $FSB_REF failed"
	git -C "$FSB_DIR" checkout -q "$FSB_REF" || die "git checkout $FSB_REF failed"
	git -C "$FSB_DIR" merge -q --ff-only "origin/$FSB_REF" \
		|| die "local $FSB_REF diverged from origin/$FSB_REF; resolve manually or point FSB_DIR elsewhere"
	FSB_COMMIT="$(git -C "$FSB_DIR" rev-parse --short HEAD)"
	log "fast-sandbox @ $FSB_REF ($FSB_COMMIT)"
	pass "fast-sandbox checkout ready"
}
FSB_COMMIT=""

# --- stage: images ---------------------------------------------------------------------

build_images() {
	log "building fast-sandbox images (controller/fastlet/fastlet-proxy/sandbox-proxy/janitor/agent)"
	local component
	for component in controller fastlet fastlet-proxy sandbox-proxy janitor firecracker-runtime-agent; do
		(cd "$FSB_DIR" && make images COMPONENT="$component" >/dev/null) \
			|| die "make images COMPONENT=$component failed"
	done
	log "building the OpenSandbox egress image ($IMG_EGRESS)"
	# Build context is the OpenSandbox repo root: the Dockerfile COPYs
	# components/egress/* and components/internal paths.
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		-f "$OSB_ROOT/components/egress/Dockerfile" -t "$IMG_EGRESS" "$OSB_ROOT" >/dev/null \
		|| die "egress image build failed"
	log "building the OpenSandbox server image ($IMG_SERVER)"
	# The server Dockerfile is self-contained under server/ (uv sync
	# against the lockfile); context is the server directory.
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
		-f "$OSB_ROOT/server/Dockerfile" -t "$IMG_SERVER" "$OSB_ROOT/server" >/dev/null \
		|| die "server image build failed"
	log "building the OpenSandbox ingress image ($IMG_INGRESS)"
	# Like egress, the ingress Dockerfile COPYs components/ingress and
	# components/internal paths, so the context is the repo root.
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
		log "XFS StateRoot already mounted at $XFS_MOUNT_POINT"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
		return 0
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
		# Mirrors are host-specific, so they are opt-in (DOCKER_MIRROR)
		# rather than baked into the committed manifest: build the
		# containerdConfigPatches block in a scratch file (one endpoint
		# ARRAY — repeated endpoint keys would override each other in
		# TOML) and insert it before `nodes:` with a two-file awk pass.
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
	# Publish the lifecycle server and the ingress gateway on the host
	# through NodePorts + extraPortMappings on the control-plane node (the
	# standard kind pattern). The mappings exist only when the cluster is
	# created with them; reusing a cluster built without them means the
	# services stay cluster-internal.
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
			docker pull -q "$KIND_NODE_IMAGE" || die "kind node image pull failed (KIND_NODE_IMAGE=$KIND_NODE_IMAGE)"
			kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" \
				${create_args+"${create_args[@]}"} --config "$kind_config" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		else
			log "creating cluster (pulling kindest/node may take minutes; set KIND_NODE_IMAGE to a mirror if it fails)"
			kind create cluster --name "$KIND_CLUSTER" \
				${create_args+"${create_args[@]}"} --config "$kind_config" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		fi
		pass "kind cluster created"
	fi
	local node
	for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
		docker exec "$node" sh -c 'test -e /dev/kvm' || die "KVM not visible inside the kind node container $node"
		kubectl label node "$node" "$KVM_NODE_LABEL=true" --overwrite >/dev/null
		kubectl label node "$node" "$FC_NODE_LABEL=true" --overwrite >/dev/null
		log "node $node: KVM + firecracker labels applied"
	done
	if [[ "$KIND_SINGLE" != "1" ]]; then
		# Multi-node kind keeps the control-plane tainted (NoSchedule),
		# which would strand half the topology: agent / fastlet / builder
		# must schedule on BOTH nodes for the P2P peer traffic to happen.
		kubectl taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null 2>&1 || true
		log "control-plane taint removed (both nodes schedulable for the P2P topology)"
	fi
	pass "kind cluster ready (KVM passthrough + labels on every node)"
}

# --- stage: MinIO + credentials ------------------------------------------------------------

minio_up() {
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	# The MinIO container writes its object store as root, so a previous
	# run's data can only be purged through sudo_.
	sudo_ rm -rf "$MINIO_DATA"
	mkdir -p "$MINIO_DATA"
	local net
	net="$(kind_network)"
	# Joining the kind network avoids docker-proxy/hairpin reachability
	# issues: pods and the node container talk to the container IP directly,
	# while 127.0.0.1 publishing keeps host-side mc/curl working.
	docker run -d --name "$MINIO_CONTAINER" --network "$net" \
		-p 127.0.0.1:"$MINIO_PORT":"$MINIO_CONTAINER_PORT" -p 127.0.0.1:"$MINIO_CONSOLE_PORT":9001 \
		-e MINIO_ROOT_USER="$MINIO_AK" -e MINIO_ROOT_PASSWORD="$MINIO_SK" \
		-v "$MINIO_DATA:/data" \
		"$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
	local attempt
	for attempt in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$MINIO_PORT/minio/health/live" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO did not become healthy"
	done
	for attempt in $(seq 1 30); do
		if mc alias set chain "http://127.0.0.1:$MINIO_PORT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO S3 API not initialized (mc alias failed)"
	done
	mc mb "chain/$MINIO_BUCKET" >/dev/null
	pass "MinIO up (bucket=$MINIO_BUCKET)"
}

resolve_minio_endpoint() {
	if [[ -n "$MINIO_ENDPOINT" ]]; then
		log "MinIO endpoint (env): $MINIO_ENDPOINT"
	else
		local net ips ip
		net="$(kind_network)"
		ips="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}} {{end}}' "$MINIO_CONTAINER")"
		ip="$(printf '%s' "$ips" | tr ' ' '\n' | grep -A1 -x "^$net$" | tail -1)"
		[[ -n "$ip" ]] || die "could not find the MinIO IP on network $net (inspect: $ips)"
		# Container port, NOT the host-published MINIO_PORT: kind-network
		# clients reach the container directly and the S3 API listens on
		# the fixed container port regardless of the host mapping.
		MINIO_ENDPOINT="http://$ip:$MINIO_CONTAINER_PORT"
		log "MinIO endpoint (kind network IP): $MINIO_ENDPOINT"
	fi
	local net
	net="$(kind_network)"
	docker run --rm --network "$net" minio/mc alias set chain \
		"$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1 \
		|| die "MinIO unreachable from the kind network at $MINIO_ENDPOINT (override MINIO_ENDPOINT)"
	pass "MinIO reachable from the kind network"
}

# gen_registry compiles the agent pull credentials through fast-sandbox's
# own registryconfig package (same pattern as its scripts/integration-env.sh).
gen_registry() { # host username password endpoint > registry.json
	mkdir -p "$FSB_GEN_DIR"
	cat > "$FSB_GEN_DIR/gen-registry.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"fast-sandbox/internal/registryconfig"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gen-registry <host> <username> <password> <endpoint>")
		os.Exit(1)
	}
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: os.Args[1], Username: os.Args[2], Password: os.Args[3], Endpoint: os.Args[4],
	}})
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
	# The platform namespace exists even if the control plane has not been
	# applied yet (resume after a partial up).
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local host
	host="${MINIO_ENDPOINT#http://}"
	host="${host#https://}"
	# Publish credentials: SecretKeyRef'd by the builder Pod (template stage).
	kubectl -n "$NS" create secret generic sandbox-oss-credentials \
		--from-literal=accessKeyId="$MINIO_AK" \
		--from-literal=secretAccessKey="$MINIO_SK" \
		--from-literal=endpoint="$MINIO_ENDPOINT" \
		--from-literal=region=us-east-1 \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the runtime-agent (compiled registryconfig).
	gen_registry "$host" "$MINIO_AK" "$MINIO_SK" "$MINIO_ENDPOINT" > "$WORK/agent-registry.json"
	jq -e . "$WORK/agent-registry.json" >/dev/null || die "generated agent registry.json is invalid"
	kubectl -n "$NS" create secret generic fast-sandbox-agent-registry \
		--from-file=registry.json="$WORK/agent-registry.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Agent endpoint override (connection address for SigV4 signing).
	kubectl -n "$NS" create configmap fast-sandbox-agent-config \
		--from-literal=artifact-endpoint="$MINIO_ENDPOINT" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the fastlet (pool-compiled registry).
	kubectl -n "$NS" create secret docker-registry registry-minio \
		--docker-server="$host" --docker-username="$MINIO_AK" --docker-password="$MINIO_SK" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=registries:
  - host: $host
    secretRef:
      name: registry-minio
" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written (publish/pull)"
}

# --- stage: control plane -------------------------------------------------------------------

control_plane_up() {
	# Canonical, code-versioned fast-sandbox manifests are applied from the
	# checkout (never duplicated here): CRDs and the all-in-one control
	# plane (namespaces, RBAC, runtime-environments ConfigMap, dev route
	# keys, single-process controller+fastpath, janitor).
	kubectl apply -k "$FSB_DIR/config/crd" >/dev/null
	kubectl apply -k "$FSB_DIR/config/all-in-one" >/dev/null
	local image
	for image in "$IMG_CONTROLLER" "$IMG_FASTLET" "$IMG_FASTLET_PROXY" \
		"$IMG_SANDBOX_PROXY" "$IMG_JANITOR" "$IMG_AGENT" "$IMG_EGRESS"; do
		kind load docker-image "$image" --name "$KIND_CLUSTER" >/dev/null
	done
	wait_for "controller deployment ready" 120 \
		kubectl -n "$NS" rollout status deploy/fast-sandbox-controller --timeout=10s
	local crd
	for crd in sandboxpools sandboxtemplates sandboxes; do
		kubectl get crd "$crd.sandbox.fast.io" >/dev/null 2>&1 || die "CRD $crd missing"
	done
	pass "CRDs + control plane ready (fast-sandbox $FSB_COMMIT)"
}

# --- stage: node assets + runtime-agent (DART P2P) ---------------------------------------------

installer_up() {
	kubectl apply -f "$MANIFESTS_DIR/node/firecracker-installer.yaml" >/dev/null
	wait_for "firecracker installer ready" 60 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-installer --timeout=15s
	pass "firecracker/jailer/kernel installed on every node"
}

agent_pods() {
	kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# render_agent_manifest replaces the "@AGENT_IMAGE@" token so IMAGE_AGENT
# overrides reach the DaemonSet (the script kind-loads $IMG_AGENT; without
# the render the DS would keep requesting the default tag).
render_agent_manifest() { # > $GEN_DIR/runtime-agent.yaml
	local src="$MANIFESTS_DIR/node/runtime-agent.yaml" out="$GEN_DIR/runtime-agent.yaml"
	mkdir -p "$GEN_DIR"
	awk -v image="$IMG_AGENT" '{ gsub(/"@AGENT_IMAGE@"/, image); print }' "$src" > "$out"
	if grep -Eq '^[[:space:]]*[A-Za-z][A-Za-z0-9]*:.*@[A-Z_]+@' "$out"; then
		die "unrendered token left in $out"
	fi
}

dart_roster_ready() { # pod expected-members
	local pod="$1" expected="$2" members
	members="$(kubectl exec -n "$NS" "$pod" -- sh -c \
		'curl -fsS --noproxy "*" http://127.0.0.1:8147/admin/members' 2>/dev/null || true)"
	[[ "$(printf '%s' "$members" | grep -o '"id":' | wc -l | tr -d ' ')" == "$expected" ]]
}

agent_up() {
	kubectl apply -f "$MANIFESTS_DIR/node/dart-service.yaml" >/dev/null
	render_agent_manifest
	kubectl apply -f "$GEN_DIR/runtime-agent.yaml" >/dev/null
	wait_for "runtime-agent DaemonSet ready" 120 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-agent --timeout=10s

	# Every agent pod must have its node-local DART child answering on the
	# admin plane, and agent /v1/health must report dartUp=true (a missing
	# dart only degrades pulls to direct S3, so this is a positive wiring
	# assertion of the default P2P data plane, not a readiness gate).
	local pod uid node pods
	pods="$(agent_pods)"
	for pod in $pods; do
		uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart admin /healthz on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' http://127.0.0.1:8147/healthz | grep -q ok"
		wait_for "agent health dartUp on $node" 30 \
			kubectl exec -n "$NS" "$pod" -- sh -c \
				"curl -fsS --noproxy '*' --unix-socket /run/fast-sandbox/firecracker/runtime.sock -H 'Content-Type: application/json' -d '{\"podUid\":\"$uid\",\"namespace\":\"$NS\"}' http://firecracker-agent/v1/health | grep -q '\"dartUp\":true'"
		log "dart: $node dart pid=$(kubectl exec -n "$NS" "$pod" -- sh -c 'pgrep -x dart')"
	done
	# P2P roster: every daemon must see every other agent pod as a peer
	# before any pull, so the second node's pull can be served by the
	# first node's dart instead of the origin.
	local expected_members
	expected_members="$(printf '%s' "$pods" | wc -w | tr -d ' ')"
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart roster full on $node ($expected_members members)" 90 \
			dart_roster_ready "$pod" "$expected_members"
	done
	pass "runtime-agent healthy + DART daemons up, roster=$expected_members (P2P default)"
}

# --- stage (server-driven): SandboxTemplate golden image -----------------------

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

# The template build is driven through the OpenSandbox server's /templates
# API: the server persists the catalog row and projects it onto
# a SandboxTemplate CRD in $NS. The build itself still runs in fast-sandbox
# (controller -> builder Pod), so the builder image must be in the cluster.
TEMPLATE_ID=""

_template_phase() {
	server_api GET "/templates/$TEMPLATE_ID" 2>/dev/null | jq -r '.status.phase // empty'
}

template_succeeded() {
	[[ "$(_template_phase)" == "Succeeded" ]]
}

template_failed() {
	local phase message
	phase="$(_template_phase)"
	[[ "$phase" == "Failed" ]] || return 1
	message="$(server_api GET "/templates/$TEMPLATE_ID" 2>/dev/null | jq -r '.status.message // empty')"
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
	local body created
	body="$(jq -n --arg image "$SBX_IMAGE" --arg publish "s3://$MINIO_BUCKET/publish" '{
		image: $image,
		publish: $publish,
		format: "native",
		resourceLimits: {cpu: "1", memory: "512Mi", disk: "2Gi"},
		readiness: {warmupSeconds: 15},
		metadata: {origin: "fast-sandbox-env"}
	}')"
	log "verify: creating the golden-image template via the server API (image=$SBX_IMAGE)"
	created="$(server_api POST /templates "$body" 2>/dev/null)" \
		|| fail "POST /templates failed against $SERVER_URL: $(curl -sS -m 60 -X POST \
			-H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" -H "Content-Type: application/json" \
			-d "$body" "$SERVER_URL/templates" 2>&1 | head -c 400)"
	TEMPLATE_ID="$(printf '%s' "$created" | jq -r '.templateId')"
	printf '%s' "$TEMPLATE_ID" > "$WORK/template-id"
	[[ -n "$TEMPLATE_ID" && "$TEMPLATE_ID" != "null" ]] || fail "template create response carried no templateId"
	log "template id=$TEMPLATE_ID"
	wait_succeeded "template phase=Succeeded" 300 template_succeeded template_failed
	local manifest_ref
	manifest_ref="$(server_api GET "/templates/$TEMPLATE_ID" | jq -r '.status.manifestRef // empty')"
	[[ -n "$manifest_ref" ]] || fail "template manifestRef is empty"
	log "template manifestRef: $manifest_ref"
	pass "SandboxTemplate Succeeded + artifacts published (via server API)"
}

# --- stage: SandboxPool (egress attached, P2P spread) --------------------------------------------

pool_pods() {
	kubectl -n "$NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

first_pool_pod() {
	kubectl -n "$NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

fastlet_pods_ready() {
	local ready=0 pod
	for pod in $(pool_pods); do
		if kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
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
		kubectl -n "$NS" get pod "$pod" -o jsonpath='{range .status.containerStatuses[*]}{.name}{"="}{.ready}{" "}{end}' 2>/dev/null \
			| grep -q 'egress=true' || return 1
	done
}

# Protocol cross-verification: GET /_fastlet/v1/actions/status must echo
# the shared apiVersion, ready=true, and a non-empty instanceId. The egress
# Handler binds Pod loopback only (127.0.0.1:18080), so the probe runs
# inside the egress container (the image ships curl).
egress_status_ready() {
	local pod out
	pod="$(first_pool_pod)"
	[[ -n "$pod" ]] || return 1
	out="$(kubectl -n "$NS" exec "pod/$pod" -c egress -- \
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
	kubectl -n "$NS" get sandboxpool "$POOL_NAME" -o jsonpath='{.status.warmImages[*].cachedFastlets}' 2>/dev/null | grep -qv '^0*$'
}

render_pool() { # > $GEN_DIR/firecracker-egress-pool.yaml
	local src="$MANIFESTS_DIR/pool/firecracker-egress-pool.yaml" out="$GEN_DIR/firecracker-egress-pool.yaml"
	mkdir -p "$GEN_DIR"
	# Tokens are quoted in the template (valid YAML); the substitution
	# replaces the quotes too, so rendered scalars keep their natural types
	# (poolMin stays an integer). awk keeps this portable across sed
	# flavors; image tags never contain awk-special replacement chars.
	awk -v fastlet="$IMG_FASTLET" -v egress="$IMG_EGRESS" \
		-v pool_min="$POOL_MIN" -v pool_max="$POOL_MAX" \
		-v warm="$WARM_IMAGES" -v image="$TEMPLATE_ID" '
		{ gsub(/"@FASTLET_IMAGE@"/, fastlet)
		  gsub(/"@EGRESS_IMAGE@"/, egress)
		  gsub(/"@POOL_MIN@"/, pool_min)
		  gsub(/"@POOL_MAX@"/, pool_max) }
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

# p2p_evidence asserts the P2P outcome from the DART block counters: the
# published artifact set (rootfs/vmstate/memory) is pulled once per 4MiB
# block from the origin cluster-wide, and when more than one node served
# traffic the second node must have been fed by the first node's peer.
p2p_evidence() { # description
	local description="$1"
	local pods pod manifest_ref manifest_key build_dir expected_blocks=0
	local origin_total=0 peer_total=0 cache_total=0 size source value active_nodes=0 node_total
	manifest_ref="$(server_api GET "/templates/$TEMPLATE_ID" 2>/dev/null | jq -r '.status.manifestRef // empty')"
	manifest_key="${manifest_ref#s3://$MINIO_BUCKET/}"
	build_dir="$(dirname "$manifest_key")"
	local object
	for object in rootfs.ext4 vmstate.snap memory.snap; do
		size="$(mc stat --json "chain/$MINIO_BUCKET/$build_dir/$object" 2>/dev/null | jq -r .size)"
		[[ "$size" =~ ^[0-9]+$ ]] || die "cannot stat published $object (publish incomplete?)"
		expected_blocks=$((expected_blocks + (size + 4194303) / 4194304))
	done
	pods="$(agent_pods)"
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

# The f1.* route-scope signing key is generated once per workdir (not per
# up run): a re-apply of the manifests must keep the key stable, or
# previously issued routes would stop verifying against the gateway.
osb_signing_key() {
	if [[ ! -s "$SIGNING_KEY_FILE" ]]; then
		openssl rand -base64 32 | tr -d '\n' > "$SIGNING_KEY_FILE"
	fi
	cat "$SIGNING_KEY_FILE"
}

# render_opensandbox replaces the @TOKEN@ placeholders of the server /
# ingress gateway manifests. Unlike the pool render (which substitutes the
# token together with its quotes so scalars keep natural YAML types), the
# quotes live in these templates: the embedded config.toml strings need
# their quotes preserved, so only the bare token is replaced.
render_opensandbox() { # <src> <out>
	local src="$1" out="$2"
	mkdir -p "$GEN_DIR"
	awk -v server_image="$IMG_SERVER" -v ingress_image="$IMG_INGRESS" \
		-v api_key="$SERVER_API_KEY" -v signing_key="$(osb_signing_key)" \
		-v fastpath="$FASTPATH_ENDPOINT" -v fleet_ns="$NS" -v pool="$POOL_NAME" \
		-v execd="$EXECD" -v gateway="$GATEWAY_ADDRESS" \
		-v server_np="$SERVER_NODEPORT" -v gateway_np="$GATEWAY_NODEPORT" '
		{ gsub(/@SERVER_IMAGE@/, server_image)
		  gsub(/@INGRESS_IMAGE@/, ingress_image)
		  gsub(/@SERVER_API_KEY@/, api_key)
		  gsub(/@SIGNING_KEY@/, signing_key)
		  gsub(/@FASTPATH_ENDPOINT@/, fastpath)
		  gsub(/@FLEETS_NAMESPACE@/, fleet_ns)
		  gsub(/@POOL_NAME@/, pool)
		  gsub(/@EXECD_IMAGE@/, execd)
		  gsub(/@GATEWAY_ADDRESS@/, gateway)
		  gsub(/@SERVER_NODEPORT@/, server_np)
		  gsub(/@GATEWAY_NODEPORT@/, gateway_np)
		  print }
	' "$src" > "$out"
	# Leftover-token guard: match only value positions (key: ...@T@...),
	# never the header comments that document the tokens themselves.
	if grep -Eq '^[[:space:]]*[A-Za-z][A-Za-z0-9]*:.*@[A-Z_]+@' "$out"; then
		die "unrendered token left in $out"
	fi
}

opensandbox_up() {
	kind load docker-image "$IMG_SERVER" --name "$KIND_CLUSTER" >/dev/null
	kind load docker-image "$IMG_INGRESS" --name "$KIND_CLUSTER" >/dev/null
	render_opensandbox "$MANIFESTS_DIR/opensandbox/server.yaml" "$GEN_DIR/osb-server.yaml"
	render_opensandbox "$MANIFESTS_DIR/opensandbox/ingress-gateway.yaml" "$GEN_DIR/osb-ingress-gateway.yaml"
	kubectl apply -f "$GEN_DIR/osb-server.yaml" >/dev/null
	kubectl apply -f "$GEN_DIR/osb-ingress-gateway.yaml" >/dev/null
	wait_for "server deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-server --timeout=10s
	wait_for "ingress gateway deployment ready" 180 \
		kubectl -n "$OSB_NS" rollout status deploy/opensandbox-ingress-gateway --timeout=10s
	# The gateway fails startup without a FastPath gRPC connection, so a
	# ready deployment already proves control-plane reachability.
	wait_for "server /health on 127.0.0.1:$SERVER_HOST_PORT" 60 \
		curl -fsS -m 5 "$SERVER_URL/health"
	wait_for "gateway /status.ok on 127.0.0.1:$GATEWAY_HOST_PORT" 60 \
		curl -fsS -m 5 "$GATEWAY_URL/status.ok"
	pass "server + ingress gateway up (fsb runtime, gateway routes signed with key 'a')"
}

# server_api wraps the lifecycle API with the configured API key.
server_api() { # method path [json-body]
	local method="$1" path="$2" body="${3:-}"
	if [[ -n "$body" ]]; then
		curl -fsS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-H "Content-Type: application/json" -d "$body" "$SERVER_URL$path"
	else
		curl -fsS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" "$SERVER_URL$path"
	fi
}

# The server assigns the sandbox id (CreateSandboxRequest carries none);
# the verify probes share it through VERIFY_ID.
VERIFY_ID=""

verify_sandbox_gone() {
	! server_api GET "/sandboxes/$VERIFY_ID" >/dev/null 2>&1
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

# opensandbox_verify drives the full wire-up end to end: server API create
# (fsb) -> FastPath -> fastlet -> firecracker sandbox (golden image with
# execd, egress attached) -> signed gateway route -> ingress ResolveEndpoint
# -> fastlet-proxy -> guest execd /ping -> delete.
# verify_one_sandbox creates one sandbox through the server API and polls
# execd /ping through the signed gateway route at 10ms intervals until 200:
# availability is measured from the client, not from the CR status chain.
# The CR state is sampled once at ping time to show observation lag.
verify_one_sandbox() { # <label> <ping-budget-ms>
	local label="$1" budget="$2" body created t0 t1 t2 route code attempt=0
	t0="$(now_ms)"
	# Default egress policy on every verify sandbox: the create carries it
	# into the egress action binding (SET_BINDING -> nft rules in the fastlet
	# Pod netns), so the policy chain is exercised on every create, not just
	# the network. /ping itself is inbound through the gateway and unaffected.
	body="$(jq -n --arg template "$TEMPLATE_ID" '{
		templateId: $template,
		timeout: 3600,
		networkPolicy: {
			defaultAction: "deny",
			egress: [
				{action: "allow", target: "example.com"},
				{action: "allow", target: "*.opensandbox.ai"}
			]
		},
		metadata: {origin: "fast-sandbox-env-verify"}
	}')"
	log "verify ($label): creating a sandbox via the server API (templateId=$TEMPLATE_ID)"
	created="$(server_api POST /sandboxes "$body" 2>/dev/null)" \
		|| fail "POST /sandboxes failed against $SERVER_URL: $(curl -sS -m 60 -X POST \
			-H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" -H "Content-Type: application/json" \
			-d "$body" "$SERVER_URL/sandboxes" 2>&1 | head -c 400)"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	t1="$(now_ms)"
	# Availability = execd /ping 200 through the signed gateway route
	# (127.0.0.1:8081 -> ingress -> ResolveEndpoint -> fastlet-proxy -> guest
	# execd :44772). Poll at 10ms; no waiting on CR status convergence.
	while :; do
		route="$(server_api GET "/sandboxes/$VERIFY_ID/endpoints/44772" 2>/dev/null \
			| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
		if [[ -n "$route" ]]; then
			code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' \
				-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" 2>/dev/null || true)"
			[[ "$code" == "200" ]] && break
		fi
		attempt=$((attempt + 1))
		if (( attempt * 10 >= budget )); then
			fail "execd /ping did not return 200 within ${budget}ms (last code=${code:-none}, route=${route:-none})"
		fi
		sleep 0.01
	done
	t2="$(now_ms)"
	# The jq filter lives in a variable first: its literal parentheses
	# inside the single-quoted program trip older bash's $( ) parser when
	# embedded in a command substitution directly.
	local state raw raw_filter
	raw_filter='{rt:.status.runtime.state,dp:.status.dataPlane.state,infra:[.status.infraComponents[]?|{n:.name,s:.state}],bind:[.status.actionBindings[]?|{h:.handler,s:.state}],ready:(.status.conditions[]?|select(.type=="Ready")|.status)}'
	state="$(server_api GET "/sandboxes/$VERIFY_ID" 2>/dev/null | jq -r '.status.state // empty')"
	raw="$(kubectl -n "$NS" get sandbox "$VERIFY_ID" -o json 2>/dev/null | jq -c "$raw_filter")"
	log "verify ($label): $VERIFY_ID access via ingress gateway: curl -H \"OpenSandbox-Ingress-To: $route\" $GATEWAY_URL/ping"
	log "verify ($label): $VERIFY_ID create POST $(( (t1 - t0) / 1000000 ))ms, POST->execd /ping 200 $(( (t2 - t1) / 1000000 ))ms (${attempt} polls @10ms), total $(( (t2 - t0) / 1000000 ))ms"
	log "verify ($label): CR at ping: server=$state raw=${raw:-unreachable}"
	pass "execd /ping 200 through the signed gateway route (44772)"
	wait_for "egress policy enforcing (networkPolicy -> egress action binding -> nft)" 120 verify_policy_enforced
	# Exercise the policy UPDATE path: PUT -> UpdateSandbox(ReplaceActionBindings)
	# -> fastlet re-SET_BINDING -> egress hot-swaps the nft rules.
	local put_body
	put_body="$(jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "github.com"}]}')"
	server_api PUT "/sandboxes/$VERIFY_ID/networkpolicy" "$put_body" >/dev/null \
		|| fail "PUT networkpolicy failed for $VERIFY_ID: $(printf '%s' "$put_body" | head -c 200)"
	wait_for "policy update converged (PUT -> ReplaceActionBindings -> egress)" 120 verify_policy_updated
	# The policy waits burn a few seconds: sample the CR state again to show
	# whether an early Failed observation converged to Running.
	local state_final
	state_final="$(server_api GET "/sandboxes/$VERIFY_ID" 2>/dev/null | jq -r '.status.state // empty')"
	log "verify ($label): $VERIFY_ID CR state final: ${state_final:-unknown}"
}

# verify_lifecycle_ops exercises the remaining sandbox lifecycle surface
# against the live stack on one sandbox: get, list, metadata merge-patch
# (upsert + delete via null), and renew-expiration.
verify_lifecycle_ops() { # <sandbox-id>
	local id="$1" out expected_expires new_expires
	out="$(server_api GET "/sandboxes/$id")" || fail "GET /sandboxes/$id failed"
	[[ "$(printf '%s' "$out" | jq -r '.id')" == "$id" ]] || fail "GET returned wrong id: $out"
	pass "lifecycle: GET /sandboxes/{id}"

	out="$(server_api GET "/sandboxes?page=1&pageSize=50")" || fail "GET /sandboxes failed"
	[[ "$(printf '%s' "$out" | jq -r --arg id "$id" '.items[]?.id | select(. == $id)' | head -1)" == "$id" ]] \
		|| fail "list does not contain $id: $(printf '%s' "$out" | jq -c '.pagination')"
	pass "lifecycle: GET /sandboxes (list contains the verify sandbox)"

	# JSON Merge Patch (RFC 7396): non-null upserts, null deletes.
	server_api PATCH "/sandboxes/$id/metadata" '{"env":"verify","stage":"lifecycle-ops"}' >/dev/null \
		|| fail "PATCH metadata upsert failed"
	out="$(server_api GET "/sandboxes/$id")"
	[[ "$(printf '%s' "$out" | jq -r '.metadata.env')" == "verify" \
		&& "$(printf '%s' "$out" | jq -r '.metadata.stage')" == "lifecycle-ops" ]] \
		|| fail "metadata upsert not visible: $(printf '%s' "$out" | jq -c '.metadata')"
	server_api PATCH "/sandboxes/$id/metadata" '{"stage":null}' >/dev/null \
		|| fail "PATCH metadata delete failed"
	out="$(server_api GET "/sandboxes/$id")"
	[[ "$(printf '%s' "$out" | jq -r '.metadata.stage')" == "null" \
		&& "$(printf '%s' "$out" | jq -r '.metadata.env')" == "verify" ]] \
		|| fail "metadata delete not visible: $(printf '%s' "$out" | jq -c '.metadata')"
	pass "lifecycle: PATCH metadata (upsert + null-delete via JSON Merge Patch)"

	# Renew: new expiresAt must be future and later than the current one.
	# The create used timeout=3600, so now+2h always qualifies.
	expected_expires="$(server_api GET "/sandboxes/$id" | jq -r '.expiresAt')"
	new_expires="$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"
	if [[ -z "$new_expires" ]]; then
		# BSD date fallback (dev hosts running macOS); GNU is authoritative.
		new_expires="$(date -u -v+2H +%Y-%m-%dT%H:%M:%SZ)"
	fi
	out="$(server_api POST "/sandboxes/$id/renew-expiration" "{\"expiresAt\": \"$new_expires\"}")" \
		|| fail "renew-expiration failed for $new_expires: $(printf '%s' "$out" | head -c 200)"
	out="$(server_api GET "/sandboxes/$id")"
	[[ "$(printf '%s' "$out" | jq -r '.expiresAt')" == "$new_expires" ]] \
		|| fail "renewed expiresAt not visible: expected $new_expires got $(printf '%s' "$out" | jq -r '.expiresAt')"
	pass "lifecycle: POST renew-expiration ($expected_expires -> $new_expires)"
}

opensandbox_verify() {
	# Cold create first: on cold fastlets it pulls the golden image through
	# DART (the slowest path, generous budget). Then warm creates: the second
	# may still pull on the OTHER node (served by the first node's DART
	# peer); once both nodes cache the set, the remaining creates must be
	# sub-second.
	local ids=() id label index
	verify_one_sandbox "cold #1" 600000
	ids+=("$VERIFY_ID")
	verify_one_sandbox "warm #2" 600000
	ids+=("$VERIFY_ID")
	for index in 3 4 5 6; do
		verify_one_sandbox "warm #$index" 120000
		ids+=("$VERIFY_ID")
	done
	pass "end-to-end: SDK API -> server -> FastPath -> fastlet -> sandbox execd -> gateway route OK (6 sandboxes)"
	verify_lifecycle_ops "${ids[0]}"
	for id in "${ids[@]}"; do
		VERIFY_ID="$id"
		server_api DELETE "/sandboxes/$id" >/dev/null \
			|| log "verify cleanup: DELETE failed; remove $id manually"
	done
	for id in "${ids[@]}"; do
		VERIFY_ID="$id"
		wait_for "verify sandbox $id deleted" 120 verify_sandbox_gone
	done
	pass "verify sandboxes cleaned up"
}

# --- status / summary ---------------------------------------------------------------------

dart_metrics_summary() {
	local pods pod node metrics
	pods="$(agent_pods 2>/dev/null || true)"
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
	echo
	log "status: SandboxPool"
	kubectl -n "$NS" get sandboxpool \
		-o custom-columns='NAME:.metadata.name,RUNTIME:.spec.runtime,READY:.status.readyPods,CAPACITY:.spec.capacity.poolMin,WARM_IMAGES:.status.warmImages' 2>/dev/null || true
	echo
	log "status: fastlet pods (egress sidecar)"
	kubectl -n "$NS" get pods -l app=sandbox-fastlet \
		-o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,CONTAINERS:.status.containerStatuses[*].name,READY:.status.containerStatuses[*].ready' 2>/dev/null || true
	echo
	log "status: DART P2P (block_source cache/peer/origin per node)"
	dart_metrics_summary || true
	echo
	log "status: MinIO"
	docker ps --filter "name=$MINIO_CONTAINER" --format '{{.Names}} {{.Status}}' 2>/dev/null || true
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
	printf '  %-22s %s\n' "fast-sandbox" "$FSB_REF @ $FSB_COMMIT ($FSB_DIR)"
	printf '  %-22s %s\n' "MinIO endpoint" "$MINIO_ENDPOINT"
	printf '  %-22s %s\n' "pool" "$POOL_NAME (runtime=firecracker, poolMin=$POOL_MIN, egress=$IMG_EGRESS)"
	printf '  %-22s %s\n' "P2P" "DART daemons=$(printf '%s' "$(agent_pods)" | wc -w | tr -d ' ') (on-demand pulls: cache -> peer -> origin)"
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
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	[[ -z "$(docker ps -a --filter "name=$MINIO_CONTAINER" --format '{{.Names}}' || true)" ]] \
		|| fail "MinIO container still present"
	# The OpenSandbox server + ingress gateway live entirely inside the kind
	# cluster and are torn down with it; only the signing key outlives it here.
	rm -f "$WORK/agent-registry.json" "$SIGNING_KEY_FILE"
	rm -rf "$GEN_DIR" "$FSB_GEN_DIR"
	# Root-owned MinIO object store (written by the container); leaving it
	# behind pollutes the host and breaks later docker build contexts.
	sudo_ rm -rf "$MINIO_DATA"
	sysctl_restore
	stateroot_xfs_down
	# Purge the per-node runtime caches the environment owns (each kind
	# node binds its own host subdirectory at /var/lib/fast-sandbox — see
	# manifests/cluster/kind-cluster.yaml). The pull layer treats a
	# committed cache as FINAL (idempotent, never refreshed), so a rebuilt
	# SandboxTemplate would otherwise keep being ignored when the StateRoot
	# survives teardown (e.g. XFS_STATEROOT=0 plain directories).
	local node_dir
	for node_dir in control-plane worker; do
		if [[ -d "$XFS_MOUNT_POINT/$node_dir/firecracker" ]]; then
			log "down: purging node runtime cache under $XFS_MOUNT_POINT/$node_dir"
			sudo_ rm -rf "$XFS_MOUNT_POINT/$node_dir/firecracker/images" \
				"$XFS_MOUNT_POINT/$node_dir/firecracker/agent" \
				"$XFS_MOUNT_POINT/$node_dir/firecracker/jails" \
				"$XFS_MOUNT_POINT/$node_dir/firecracker/cache" 2>/dev/null || true
		fi
	done
	pass "host cleanup complete"
}

# --- main --------------------------------------------------------------------------------------

usage() {
	cat <<'EOF'
usage: fast-sandbox-env.sh [--auto-clean] {up|down|status|pool}

  up       initialize the full environment: fast-sandbox@master images,
           two-node kind cluster (KVM), MinIO, control plane, firecracker
           node assets, runtime-agent + DART (P2P), SandboxTemplate golden
           image, firecracker-egress-pool (egress attached), the
           source-built OpenSandbox server + ingress gateway, and an
           end-to-end verify (create -> gateway route -> execd /ping).
  pool     re-apply only the SandboxPool (after editing manifests/pool/)
  status   nodes / pods / pool / DART P2P counters / MinIO / OpenSandbox health
  down     teardown: kind cluster + MinIO + sysctl + XFS StateRoot + caches

  --auto-clean  on up failure, run down automatically before dumping logs

Notable env overrides: WORK, FSB_DIR, KIND_CLUSTER, KIND_SINGLE,
DOCKER_MIRROR, MINIO_*, EGRESS_IMAGE, SERVER_IMAGE, INGRESS_IMAGE,
IMAGE_<COMPONENT>, POOL_MIN/POOL_MAX, WARM_IMAGES=1, SBX_IMAGE, EXECD,
XFS_STATEROOT=0, SKIP_TOOL_INSTALL=1, SKIP_LEFTOVER_CLEAN=1.
See the header of this script.
EOF
	exit 1
}

for arg in "$@"; do
	case "$arg" in
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|pool) ACTION="$arg" ;;
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
			echo "cluster=$KIND_CLUSTER single=$KIND_SINGLE minio=$MINIO_IMAGE port=$MINIO_PORT bucket=$MINIO_BUCKET"
			echo "sbxImage=$SBX_IMAGE execd=$EXECD warmImages=$WARM_IMAGES"
			echo "pool=$POOL_NAME poolMin=$POOL_MIN egress=$IMG_EGRESS"
			echo "server=$IMG_SERVER ingress=$IMG_INGRESS fastpath=$FASTPATH_ENDPOINT"
			echo "images: controller=$IMG_CONTROLLER agent=$IMG_AGENT"
		} > "$LOGS_DIR/environment.txt" 2>&1 || true
		if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
			|| docker ps -a --format '{{.Names}}' | grep -qx "$MINIO_CONTAINER"; then
			if [[ "$SKIP_LEFTOVER_CLEAN" == 1 ]]; then
				log "leftover resources detected; aborting (SKIP_LEFTOVER_CLEAN=1). Run 'fast-sandbox-env.sh down' first"
				exit 1
			fi
			log "leftover resources detected; cleaning and rebuilding"
			down
		fi
		trap 'on_error up' ERR
		run_stage "preflight + tooling" preflight
		run_stage "sysctl (fs.inotify)" sysctl_set
		run_stage "fast-sandbox checkout @$FSB_REF" ensure_fsb
		run_stage "build images (fast-sandbox + OpenSandbox)" build_images
		run_stage "XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "render kind config" render_kind_config
		run_stage "kind cluster (KVM passthrough + labels)" kind_up
		run_stage "MinIO + bucket" minio_up
		run_stage "MinIO endpoint (kind network)" resolve_minio_endpoint
		run_stage "CRDs + control plane" control_plane_up
		run_stage "credentials (publish/pull)" credentials_up
		run_stage "firecracker node assets" installer_up
		run_stage "runtime-agent + DART (P2P)" agent_up
		run_stage "OpenSandbox server + ingress gateway" opensandbox_up
		run_stage "SandboxTemplate build (server API)" template_up
		run_stage "SandboxPool $POOL_NAME (egress + P2P)" pool_up
		run_stage "end-to-end verify (templateId create -> gateway -> execd /ping)" opensandbox_verify
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
	down)
		down
		;;
esac
