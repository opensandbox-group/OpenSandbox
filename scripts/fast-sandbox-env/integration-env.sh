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

# integration-env.sh — one-command fast-sandbox integration environment,
# driven from the OpenSandbox repository.
#
# Builds the full OpenSandbox ecosystem on a bare-metal Linux KVM host
# (fast-sandbox checked out at the commit pinned in
# manifests/third-party/fast-sandbox.commit): two-node kind cluster with KVM
# passthrough → Helm charts/base (sandbox.fast.io CRDs + component RBAC) +
# charts/fast-sandbox (all-in-one control plane, janitor, node installer,
# firecracker runtime readiness + DART) → MinIO artifact store → the firecracker-egress-pool
# SandboxPool with the OpenSandbox egress sidecar attached through the
# Sandbox Actions channel → the source-built OpenSandbox lifecycle server
# (fsb runtime) and ingress gateway via charts/server and
# charts/ingress-gateway → an end-to-end verify (create through the server
# API, execd /ping through the signed gateway route, delete); then
# pause/resume (checkpoint to the artifact store, capacity released,
# resume) and a public-snapshot verify (snapshot a Running sandbox, watch
# it to Ready, restore a NEW sandbox from the snapshotId and boot it).
#
# All Kubernetes resources come from the OpenSandbox Helm charts
# (manifests/charts), rendered with `helm template` and applied with plain
# `kubectl apply` — helm is only a renderer here, the cluster keeps no helm
# state, and re-runs keep the idempotent apply semantics. The only
# env-owned manifests left are the kind cluster config and the SandboxPool
# resource.
#
# Usage:
#   ./scripts/fast-sandbox-env/integration-env.sh up       # full environment + pool + server/ingress + verify
#   ./scripts/fast-sandbox-env/integration-env.sh pool     # re-apply the pool only
#   ./scripts/fast-sandbox-env/integration-env.sh status   # component/pool/DART/OpenSandbox health
#   ./scripts/fast-sandbox-env/integration-env.sh down     # teardown, host left clean
#   ./scripts/fast-sandbox-env/integration-env.sh up --auto-clean   # down on failure
#
# Environment overrides (all optional):
#   WORK                 workspace + logs        (default $PWD/.fast-sandbox-env)
#   FSB_DIR              fast-sandbox checkout  (default $WORK/fast-sandbox —
#                        env-owned clone; source pinned in manifests/third-party/fast-sandbox.commit)
#   WORK                  workspace root        (default /data/fast-sandbox-env when /data exists, else $PWD/.fast-sandbox-env)
#   KIND_CLUSTER / KIND_NODE_IMAGE / KIND_RETAIN / KIND_SINGLE
#   DOCKER_MIRROR        comma list injected as docker.io containerd mirrors
#   MINIO_PORT / MINIO_CONSOLE_PORT / MINIO_AK / MINIO_SK / MINIO_IMAGE / MC_IMAGE / MINIO_ENDPOINT
#   IMAGE_<NAME>         fast-sandbox component image tags
#   EGRESS_IMAGE         egress image tag        (default docker.io/opensandbox/egress:latest)
#   SERVER_IMAGE / INGRESS_IMAGE  OpenSandbox server/ingress image tags
#   SERVER_HOST_PORT / GATEWAY_HOST_PORT  host-side publishes (default 18080/18081)
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
# Heavy run-state belongs on a data volume, not the repo/root disk.
if [[ -d /data ]] && [[ -z "${WORK:-}" ]]; then
	WORK="/data/fast-sandbox-env"
else
	WORK="${WORK:-$PWD/.fast-sandbox-env}"
fi
LOGS_DIR="$WORK/logs"
GEN_DIR="$WORK/gen"

# Env-owned fast-sandbox clone under $WORK (independent of any checkout
# outside the workspace); FSB_DIR only relocates the clone. The source is
# exactly the commit pinned in manifests/third-party/fast-sandbox.commit —
# there is no ref override; to test a different source, bump the pin.
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

MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:latest}"
MC_IMAGE="${MC_IMAGE:-minio/mc:latest}"
MINIO_PORT="${MINIO_PORT:-19000}"
# The container always LISTENS on 9000 (guest side of the publish map and
# the port kind-network clients use via the container IP); MINIO_PORT only
# moves the host-side 127.0.0.1 publish.
MINIO_CONTAINER_PORT=9000
# Console (human-only UI) listens on 9001 in-container; the host-side
# publish defaults to 19001: 9000/9001 are common host-port collisions.
MINIO_CONSOLE_PORT="${MINIO_CONSOLE_PORT:-19001}"
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
IMG_CONTROLLER="${IMAGE_CONTROLLER:-opensandbox/fsb-controller:dev}"
IMG_FASTLET="${IMAGE_FASTLET:-opensandbox/fsb-fastlet:dev}"
IMG_FASTLET_PROXY="${IMAGE_FASTLET_PROXY:-opensandbox/fsb-fastlet-proxy:dev}"
IMG_JANITOR="${IMAGE_JANITOR:-opensandbox/fsb-janitor:dev}"
IMG_BUILDER="${IMAGE_BUILDER:-opensandbox/fsb-sandboxtemplate-builder:dev}"
IMG_RUNTIME="${IMAGE_RUNTIME:-opensandbox/fsb-firecracker-runtime:dev}"
IMG_EGRESS="${EGRESS_IMAGE:-docker.io/opensandbox/egress:latest}"

image_repo() { printf '%s' "${1%:*}"; }
image_tag() { printf '%s' "${1##*:}"; }

# --- OpenSandbox server + ingress gateway (source-built) ----------------------
# Fixed shape of this environment: no knobs, the full stack always runs.

OSB_NS="opensandbox-system"
IMG_SERVER="${SERVER_IMAGE:-docker.io/opensandbox/server:env}"
IMG_INGRESS="${INGRESS_IMAGE:-docker.io/opensandbox/ingress:env}"
# FastPath v2 of this cluster's all-in-one control plane (in-cluster DNS).
FASTPATH_ENDPOINT="fast-sandbox-fastpath.opensandbox-system.svc:9090"
SERVER_API_KEY="fast-sandbox-env"
# Shared f1.* route-scope signing key (server [ingress.secure_access] and
# ingress --secure-access-keys), generated once per workdir so re-applies
# keep previously issued routes verifiable.
SIGNING_KEY_FILE="$WORK/opensandbox-signing-key"
# Host-side publish (kind extraPortMappings on the control-plane node, bound
# to 127.0.0.1 only): Service NodePorts -> server :80 / gateway :28888.
# Overridable because host port collisions are environment-specific.
SERVER_HOST_PORT="${SERVER_HOST_PORT:-18080}"
GATEWAY_HOST_PORT="${GATEWAY_HOST_PORT:-18081}"
SERVER_NODEPORT=30880
GATEWAY_NODEPORT=30881
GATEWAY_ADDRESS="127.0.0.1:$GATEWAY_HOST_PORT"
SERVER_URL="http://127.0.0.1:$SERVER_HOST_PORT"
GATEWAY_URL="http://127.0.0.1:$GATEWAY_HOST_PORT"

# Node labels: the firecracker-runtime readiness loop applies both itself
# (sandbox.fast.io/kvm is hardcoded by the SandboxTemplate reconciler;
# fast-sandbox.io/firecracker-node gates the fastlet scheduling).
KVM_NODE_LABEL="sandbox.fast.io/kvm"
FC_NODE_LABEL="fast-sandbox.io/firecracker-node"

INOTIFY_VALUE="${INOTIFY_VALUE:-8192}"
SYSCTL_BACKUP="$WORK/sysctl-backup"
SKIP_TOOL_INSTALL="${SKIP_TOOL_INSTALL:-0}"
SKIP_LEFTOVER_CLEAN="${SKIP_LEFTOVER_CLEAN:-0}"
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.0}"
HELM_VERSION="${HELM_VERSION:-v3.16.4}"

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

mc() { docker run --rm --network host -v "$WORK/mc-config:/root/.mc" "$MC_IMAGE" "$@"; }

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
	# kind requires cgroup v2: on v1 hosts kubelet fails to create the
	# kubepods cgroup regardless of the docker cgroup driver.
	if [[ "$cgver" == "1" ]]; then
		die "docker cgroup Version is 1; kind requires cgroup v2. Enable it with the kernel cmdline 'systemd.unified_cgroup_hierarchy=1' and reboot"
	fi
	[[ -e /dev/kvm ]] || die "/dev/kvm is missing on this host (KVM required)"
	# Fail fast per heavy-data target instead of ENOSPC mid-run.
	local min_free_kb=$((20 * 1024 * 1024)) avail_kb target xfs_dir
	xfs_dir="${XFS_LOOP_FILE%/*}"
	mkdir -p "$WORK" "$MINIO_DATA" "$xfs_dir" 2>/dev/null || true
	local -a targets=("$WORK" "$MINIO_DATA" "$xfs_dir")
	for target in "${targets[@]}"; do
		avail_kb="$(df -Pk "$target" 2>/dev/null | awk 'NR==2 {print $4}')"
		[[ "$avail_kb" =~ ^[0-9]+$ ]] || die "cannot determine free disk space on $target"
		if (( avail_kb < min_free_kb )); then
			die "$target has $((avail_kb / 1024 / 1024))G free; at least 20G is required (built images, XFS StateRoot, MinIO artifacts). Free space (docker system prune / old kind clusters) or point WORK / MINIO_DATA / XFS_LOOP_FILE at a bigger volume"
		fi
		log "disk headroom: $target has $((avail_kb / 1024 / 1024 / 1024))G free"
	done
	# Fail fast on busy host ports instead of dying at the docker bind or
	# kind create. MinIO culprits: a leftover MinIO container; 8080/8081 are
	# published by the kind node for the server / ingress gateway.
	local port
	for port in "$MINIO_PORT" "$MINIO_CONSOLE_PORT" "$SERVER_HOST_PORT" "$GATEWAY_HOST_PORT"; do
		if host_port_busy "$port"; then
			die "127.0.0.1:$port is already in use (check 'ss -ltnp' / 'docker ps'); free it, or set MINIO_PORT / MINIO_CONSOLE_PORT / SERVER_HOST_PORT / GATEWAY_HOST_PORT"
		fi
	done
	ensure_image "$MINIO_IMAGE"
	ensure_image "$MC_IMAGE"
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
		log "cloning $FSB_REPO into $FSB_DIR"
		git clone "$FSB_REPO" "$FSB_DIR" || die "clone failed; check network"
	fi
	rm -rf "$FSB_GEN_DIR"
	[[ -z "$(git -C "$FSB_DIR" status --porcelain)" ]] \
		|| die "fast-sandbox checkout at $FSB_DIR has local changes; delete it to re-clone or point FSB_DIR at a clean checkout"
	# A raw SHA fetch needs allow-reachable-sha1-in-want (GitHub supports
	# it); fall back to a full ref fetch for other remotes.
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
	# Same checkout, same defaults (fast-sandbox/<component>:dev) as the
	# standalone builder, so the env and the published build path cannot
	# drift. boxlite / sandbox-action-fixture / sandbox-proxy are never
	# built.
	FSB_SRC_DIR="$FSB_DIR" "$OSB_ROOT/manifests/release/build-fast-sandbox.sh" \
		|| die "fast-sandbox image build failed"
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
			ensure_image "$KIND_NODE_IMAGE" || die "kind node image unavailable locally and pull failed (KIND_NODE_IMAGE=$KIND_NODE_IMAGE)"
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
		log "node $node: /dev/kvm visible"
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

# gen_registry compiles the agent registry via fast-sandbox's registryconfig
# package; the optional write pair covers checkpoint/snapshot publication.
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
	# The platform namespace exists even if the control plane has not been
	# applied yet (resume after a partial up).
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl create namespace "$RESOURCE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local host
	host="${MINIO_ENDPOINT#http://}"
	host="${host#https://}"
	# Publish credentials: SecretKeyRef'd by the builder Pod (template
	# stage); builder Pods run next to their SandboxTemplate in the
	# dataplane namespace.
	kubectl -n "$RESOURCE_NS" create secret generic sandbox-oss-credentials \
		--from-literal=accessKeyId="$MINIO_AK" \
		--from-literal=secretAccessKey="$MINIO_SK" \
		--from-literal=endpoint="$MINIO_ENDPOINT" \
		--from-literal=region=us-east-1 \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Agent pull+publish credentials (the write pair covers checkpoints).
	gen_registry "$host" "$MINIO_AK" "$MINIO_SK" "$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" \
		> "$WORK/agent-registry.json"
	jq -e '.credentials[0].writeUsername' "$WORK/agent-registry.json" >/dev/null \
		|| die "generated agent registry carries no write credential"
	kubectl -n "$NS" create secret generic fast-sandbox-agent-registry \
		--from-file=registry.json="$WORK/agent-registry.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# The fast-sandbox-artifact-store ConfigMap (store + live endpoint) is
	# rendered by charts/fast-sandbox at install time.
	# Pull credentials for the fastlet (pool-compiled registry); fastlets
	# run in the dataplane namespace.
	kubectl -n "$RESOURCE_NS" create secret docker-registry registry-minio \
		--docker-server="$host" --docker-username="$MINIO_AK" --docker-password="$MINIO_SK" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$RESOURCE_NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=registries:
  - host: $host
    secretRef:
      name: registry-minio
" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written (publish/pull)"
}

# --- stage: control plane -------------------------------------------------------------------

# The charts are rendered with `helm template` and applied with kubectl:
# helm stays a renderer, the cluster keeps no helm release state, and
# re-runs keep the plain idempotent `kubectl apply` semantics.
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
	# The OpenSandbox Helm charts are the source of truth: charts/base ships
	# the sandbox.opensandbox.io + sandbox.fast.io CRDs, the component RBAC
	# and the namespaces; charts/fast-sandbox ships the all-in-one control
	# plane (reconcilers + FastPath), the janitor, the node installer and
	# the runtime-agent. This replaces the fast-sandbox checkout's
	# config/crd + config/all-in-one kustomize applies and the env-owned
	# node manifests.
	apply_ns "$NS"
	helm_render fsb-base "$OSB_ROOT/manifests/charts/base" "$NS" "$GEN_DIR/fsb-base.yaml"
	kubectl apply -f "$GEN_DIR/fsb-base.yaml" >/dev/null
	helm_render fast-sandbox "$OSB_ROOT/manifests/charts/fast-sandbox" "$NS" \
		"$GEN_DIR/fast-sandbox.yaml" \
		--set controller.image.repository="$(image_repo "$IMG_CONTROLLER")" \
		--set controller.image.tag="$(image_tag "$IMG_CONTROLLER")" \
		--set controller.sandboxtemplateBuilderImage="$IMG_BUILDER" \
		--set janitor.image.repository="$(image_repo "$IMG_JANITOR")" \
		--set janitor.image.tag="$(image_tag "$IMG_JANITOR")" \
		--set runtime.image.repository="$(image_repo "$IMG_RUNTIME")" \
		--set runtime.image.tag="$(image_tag "$IMG_RUNTIME")" \
		--set artifactStore.store="s3://$MINIO_BUCKET/publish" \
		--set artifactStore.endpoint="$MINIO_ENDPOINT"
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
	# The firecracker-runtime DaemonSet + dart headless Service ship with
	# the charts/fast-sandbox release (artifact-store endpoint was pinned
	# at install time; the registry Secret lands in credentials_up before
	# this stage).
	wait_for "firecracker-runtime DaemonSet ready" 120 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime --timeout=10s

	# Every runtime pod must have its node-local DART child answering on the
	# admin plane, and agent /v1/health must report dartUp=true (a missing
	# dart only degrades pulls to direct S3, so this is a positive wiring
	# assertion of the default P2P data plane, not a readiness gate).
	local pod uid node pods
	pods="$(runtime_pods)"
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
	# P2P roster: every daemon must see every other runtime pod as a peer
	# before any pull, so the second node's pull can be served by the
	# first node's dart instead of the origin.
	local expected_members
	expected_members="$(printf '%s' "$pods" | wc -w | tr -d ' ')"
	for pod in $pods; do
		node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
		wait_for "dart roster full on $node ($expected_members members)" 90 \
			dart_roster_ready "$pod" "$expected_members"
	done
	# Node readiness: the readiness loop verifies each host, installs the
	# Firecracker assets and applies the scheduling labels +
	# FirecrackerReady condition itself (the old manual kubectl label step
	# is gone).
	for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
		wait_for "node $node labeled + FirecrackerReady" 120 \
			runtime_node_labeled "$node"
	done
	pass "firecracker-runtime healthy + DART roster=$expected_members + nodes FirecrackerReady"
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

# Protocol cross-verification: GET /_fastlet/v1/actions/status must echo
# the shared apiVersion, ready=true, and a non-empty instanceId. The egress
# Handler binds Pod loopback only (127.0.0.1:18080), so the probe runs
# inside the egress container (the image ships curl).
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

# The f1.* route-scope signing key is generated once per workdir (not per
# up run): a re-apply of the manifests must keep the key stable, or
# previously issued routes would stop verifying against the gateway.
osb_signing_key() {
	if [[ ! -s "$SIGNING_KEY_FILE" ]]; then
		openssl rand -base64 32 | tr -d '\n' > "$SIGNING_KEY_FILE"
	fi
	cat "$SIGNING_KEY_FILE"
}

# render_server_config writes the lifecycle server's config.toml (fsb
# runtime) with the workdir's tokens substituted. The gateway-mode ingress
# section is NOT part of it: charts/server appends its own [ingress] block
# rendered from server.gateway.* values (see opensandbox_up).
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
# One block serves both backends: CR reads and the
# fsb (fast-sandbox) settings. Sandboxes are created in the pool's
# namespace so poolRef resolves; execd comes from runtime.execd_image
# above (the server injects it into server-created SandboxTemplates).
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
	# charts/server: fsb RBAC (sandbox.fast.io reads + SandboxTemplate
	# management) is built in; the config carries the fsb runtime wiring.
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
	# charts/ingress-gateway: fast-sandbox provider resolving through
	# FastPath, verifying the same signing key the server signs with.
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
# The snapshot verify stage shares the created snapshot ids through
# SNAPSHOT_ID / SNAPSHOT_ID2.
SNAPSHOT_ID=""
SNAPSHOT_ID2=""
# A re-entry snapshot accepted during fence cache lag (202) is tracked here
# so its terminal outcome is asserted and it is cleaned up.
SNAPSHOT_EXTRA=""

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
	raw="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o json 2>/dev/null | jq -c "$raw_filter")"
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

	# Capture the body: a 503 here carries the backend error code that
	# names the failing list source.
	local list_code
	list_code="$(curl -sS -m 60 -o "$WORK/last-list.json" -w '%{http_code}' \
		-H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
		"$SERVER_URL/sandboxes?page=1&pageSize=50")"
	if [[ "$list_code" != "200" ]] || ! jq -e '.items' "$WORK/last-list.json" >/dev/null 2>&1; then
		fail "GET /sandboxes returned $list_code: $(head -c 400 "$WORK/last-list.json" 2>/dev/null)"
	fi
	out="$(cat "$WORK/last-list.json")"
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

# --- stage: pause / resume (server API -> FastPath checkpoint) ------------------

# Fresh signed route + one GET: doubles as the "runtime actually serving" probe.
execd_ping_ok() {
	local route code
	route="$(server_api GET "/sandboxes/$VERIFY_ID/endpoints/44772" 2>/dev/null \
		| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
	[[ -n "$route" ]] || return 1
	code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' \
		-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" 2>/dev/null || true)"
	[[ "$code" == "200" ]]
}

sandbox_state_is() { # <state>
	[[ "$(server_api GET "/sandboxes/$VERIFY_ID" 2>/dev/null | jq -r '.status.state // empty')" == "$1" ]]
}

sandbox_running() { sandbox_state_is Running; }
sandbox_paused() { sandbox_state_is Paused; }

pause_resume_verify() {
	# 202 + poll GET: Paused == checkpoint durable + capacity released;
	# resume advances the route generation, so /ping needs a fresh route.
	local body created t0 t1 t2 t3
	body="$(jq -n --arg template "$TEMPLATE_ID" '{
		templateId: $template,
		timeout: 3600,
		metadata: {origin: "fast-sandbox-env-pause"}
	}')"
	log "verify (pause): creating a sandbox via the server API (templateId=$TEMPLATE_ID)"
	t0="$(now_ms)"
	created="$(server_api POST /sandboxes "$body" 2>/dev/null)" \
		|| fail "POST /sandboxes failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "pause target Running" 150 sandbox_running
	t1="$(now_ms)"
	log "verify (pause): $VERIFY_ID create->Running $(( (t1 - t0) / 1000000 ))ms"

	wait_for "pre-pause execd /ping 200 through the gateway" 100 execd_ping_ok

	log "verify (pause): POST /sandboxes/$VERIFY_ID/pause"
	server_api POST "/sandboxes/$VERIFY_ID/pause" >/dev/null \
		|| fail "POST pause failed for $VERIFY_ID"
	# Paused is durable-first: the checkpoint must be complete in the artifact
	# store before the state is reported; the dump itself keeps serving.
	wait_for "poll GET until Paused (checkpoint durable, capacity released)" 240 sandbox_paused
	t2="$(now_ms)"
	log "verify (pause): $VERIFY_ID pause POST->Paused (checkpoint durable) $(( (t2 - t1) / 1000000 ))ms"
	if execd_ping_ok; then
		fail "paused sandbox still serves /ping through the gateway"
	fi
	pass "paused: execd /ping no longer served (runtime released, signed route rejected at the gateway)"

	log "verify (pause): POST /sandboxes/$VERIFY_ID/resume"
	t2="$(now_ms)"
	server_api POST "/sandboxes/$VERIFY_ID/resume" >/dev/null \
		|| fail "POST resume failed for $VERIFY_ID"
	wait_for "poll GET until Running (checkpoint restored)" 240 sandbox_running
	t3="$(now_ms)"
	log "verify (pause): $VERIFY_ID resume POST->Running (checkpoint restored) $(( (t3 - t2) / 1000000 ))ms"
	wait_for "post-resume execd /ping 200 through a fresh route" 100 execd_ping_ok
	pass "pause/resume round-trip timings: pause->Paused $(( (t2 - t1) / 1000000 ))ms, resume->Running $(( (t3 - t2) / 1000000 ))ms (server API -> FastPath -> artifact store)"

	server_api DELETE "/sandboxes/$VERIFY_ID" >/dev/null \
		|| log "verify cleanup: DELETE failed; remove $VERIFY_ID manually"
	wait_for "pause/resume sandbox deleted" 120 verify_sandbox_gone
}

# --- stage: snapshot (server API -> SandboxSnapshot CR -> restore) --------------

# Ready when the server watcher has converged the snapshot row from the
# fast-sandbox SandboxSnapshot CR (Succeeded + template index published).
snapshot_ready() { # <snapshot-id>
	[[ "$(server_api GET "/snapshots/$1" 2>/dev/null | jq -r '.status.state // empty')" == "Ready" ]]
}

# Terminal (Ready or Failed): the fastlet pause-window fence resolves an
# accepted-but-conflicting snapshot one way or the other.
snapshot_terminal() { # <snapshot-id>
	local state
	state="$(server_api GET "/snapshots/$1" 2>/dev/null | jq -r '.status.state // empty')"
	[[ "$state" == "Ready" || "$state" == "Failed" ]]
}

snapshot_verify() {
	# Full public-snapshot round trip on the live stack: create a sandbox,
	# POST a snapshot (202 + Creating), let the server watcher converge the
	# row from the SandboxSnapshot CR, restore a NEW sandbox from the
	# snapshotId (the published template index becomes its rootfs artifact
	# set), and prove the restored sandbox boots by execd /ping through the
	# signed gateway route. Also covers: re-entry rejection while the dump
	# window holds the sandbox, source-sandbox survival across the pause
	# window, and a second (terminal-fenced) snapshot of the same sandbox.
	local body created out source_id snapshot_id snapshot_id2 restore_id reentry_out reentry_code
	local t0 t1 t2 t3 t4
	body="$(jq -n --arg template "$TEMPLATE_ID" '{
		templateId: $template,
		timeout: 3600,
		metadata: {origin: "fast-sandbox-env-snapshot"}
	}')"
	t0="$(now_ms)"
	log "verify (snapshot): creating the source sandbox via the server API (templateId=$TEMPLATE_ID)"
	created="$(server_api POST /sandboxes "$body" 2>/dev/null)" \
		|| fail "POST /sandboxes failed against $SERVER_URL"
	source_id="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$source_id" && "$source_id" != "null" ]] || fail "create response carried no id"
	VERIFY_ID="$source_id"
	wait_for "snapshot source sandbox Running" 300 sandbox_running
	wait_for "pre-snapshot execd /ping 200 through the gateway" 100 execd_ping_ok
	t1="$(now_ms)"
	log "verify (snapshot): source sandbox $source_id create->Running $(( (t1 - t0) / 1000000 ))ms"

	# 1. Snapshot create: 202 + Creating; the dump holds the runtime pause
	# window, artifacts publish after it; the server row converges from the
	# SandboxSnapshot CR via its watcher.
	log "verify (snapshot): POST /sandboxes/$source_id/snapshots"
	t1="$(now_ms)"
	out="$(server_api POST "/sandboxes/$source_id/snapshots" '{"name":"env-verify"}' 2>/dev/null)" \
		|| fail "POST snapshots failed for $source_id"
	SNAPSHOT_ID="$(printf '%s' "$out" | jq -r '.id')"
	[[ -n "$SNAPSHOT_ID" && "$SNAPSHOT_ID" != "null" ]] || fail "snapshot create carried no id"
	[[ "$(printf '%s' "$out" | jq -r '.status.state')" == "Creating" ]] \
		|| fail "snapshot create did not return Creating: $(printf '%s' "$out" | head -c 300)"

	# 2. Re-entry: a second snapshot POST while the first holds the dump
	# window is fenced by FastPath (FailedPrecondition -> 409) once the CR
	# is cache-visible; with watcher cache lag the POST is accepted (202)
	# and the fastlet pause window — the authoritative fence — resolves the
	# extra snapshot to a terminal phase after the first completes.
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

	wait_for "poll GET /snapshots/$SNAPSHOT_ID until Ready (watcher -> SandboxSnapshot CR -> store index)" 300 snapshot_ready "$SNAPSHOT_ID"
	t2="$(now_ms)"
	log "verify (snapshot): $SNAPSHOT_ID snapshot POST->Ready $(( (t2 - t1) / 1000000 ))ms"
	pass "snapshot: POST 202 Creating -> watcher -> Ready"

	# 3. Source survival: the pause window must be released and the sandbox
	# back to serving after the snapshot reached its terminal phase.
	VERIFY_ID="$source_id"
	wait_for "source sandbox Running again after the snapshot" 120 sandbox_running
	wait_for "source sandbox execd /ping 200 after the snapshot" 100 execd_ping_ok
	pass "snapshot: source sandbox survived (Running + /ping 200)"

	# 3b. An accepted re-entry snapshot (fence cache lag) must reach a
	# terminal phase once the first snapshot releases the pause window — a
	# stuck non-terminal snapshot would block every future snapshot of the
	# sandbox through the re-entry fence.
	if [[ -n "$SNAPSHOT_EXTRA" ]]; then
		wait_for "re-entry snapshot $SNAPSHOT_EXTRA reaches a terminal phase" 150 snapshot_terminal "$SNAPSHOT_EXTRA"
		log "verify (snapshot): re-entry snapshot $SNAPSHOT_EXTRA terminal: $(server_api GET "/snapshots/$SNAPSHOT_EXTRA" 2>/dev/null | jq -r '.status.state')"
		pass "snapshot: accepted re-entry snapshot resolved to a terminal phase"
	fi

	# 4. Second snapshot after the first is terminal: a fresh id, fenced in
	# by re-entry only while non-terminal.
	t3="$(now_ms)"
	out="$(server_api POST "/sandboxes/$source_id/snapshots" '{"name":"env-verify-2"}' 2>/dev/null)" \
		|| fail "second snapshot POST failed for $source_id"
	snapshot_id2="$(printf '%s' "$out" | jq -r '.id')"
	[[ -n "$snapshot_id2" && "$snapshot_id2" != "null" && "$snapshot_id2" != "$SNAPSHOT_ID" ]] \
		|| fail "second snapshot did not produce a distinct id: $snapshot_id2"
	SNAPSHOT_ID2="$snapshot_id2"
	wait_for "poll GET /snapshots/$snapshot_id2 until Ready" 300 snapshot_ready "$snapshot_id2"
	t4="$(now_ms)"
	log "verify (snapshot): $snapshot_id2 second snapshot POST->Ready $(( (t4 - t3) / 1000000 ))ms"
	pass "snapshot: repeated snapshot of the same sandbox -> Ready (distinct id)"

	# 5. Listing: sandboxId scoping returns both required snapshots, both
	# Ready (an accepted re-entry snapshot may add a third, terminal row).
	out="$(server_api GET "/snapshots?sandboxId=$source_id&pageSize=50")"
	[[ "$(printf '%s' "$out" | jq -r --arg id "$SNAPSHOT_ID" --arg id2 "$snapshot_id2" \
		'[.items[] | select((.id == $id or .id == $id2) and .status.state == "Ready")] | length')" == "2" ]] \
		|| fail "snapshot list does not contain both required snapshots as Ready: $(printf '%s' "$out" | head -c 400)"
	if [[ -n "$SNAPSHOT_EXTRA" ]]; then
		[[ "$(printf '%s' "$out" | jq -r --arg id "$SNAPSHOT_EXTRA" \
			'[.items[] | select(.id == $id)] | length')" == "1" ]] \
			|| fail "accepted re-entry snapshot $SNAPSHOT_EXTRA missing from the list"
	fi
	pass "snapshot: list scoped by sandboxId contains the snapshots (Ready)"

	# 6. Restore: the snapshot row resolves to the published template index
	# key (osb-snap-<uuid hex>); the restored sandbox boots that artifact
	# set. resourceLimits must restate the pool profile (firecracker pool).
	local restore_body restored
	restore_body="$(jq -n --arg snapshot "$SNAPSHOT_ID" '{
		snapshotId: $snapshot,
		timeout: 3600,
		resourceLimits: {cpu: "1", memory: "512Mi", pids: "128"}
	}')"
	log "verify (snapshot): POST /sandboxes with snapshotId=$SNAPSHOT_ID"
	t3="$(now_ms)"
	restored="$(server_api POST /sandboxes "$restore_body" 2>/dev/null)" \
		|| fail "POST /sandboxes (snapshotId=$SNAPSHOT_ID) failed"
	restore_id="$(printf '%s' "$restored" | jq -r '.id')"
	[[ -n "$restore_id" && "$restore_id" != "null" ]] || fail "restore create carried no id"
	VERIFY_ID="$restore_id"
	wait_for "restored sandbox Running" 600 sandbox_running
	t4="$(now_ms)"
	wait_for "restored execd /ping 200 through the gateway" 300 execd_ping_ok
	log "verify (snapshot): restored $restore_id POST->Running $(( (t4 - t3) / 1000000 ))ms"
	pass "snapshot: restore -> sandbox boots the published artifact set -> execd /ping OK"

	# 7. Cleanup: restored sandbox, source sandbox, then the snapshot rows
	# (the server forwards artifact deletion through DeleteSandboxSnapshot).
	server_api DELETE "/sandboxes/$restore_id" >/dev/null \
		|| log "verify cleanup: DELETE $restore_id failed"
	wait_for "restored sandbox deleted" 120 verify_sandbox_gone
	VERIFY_ID="$source_id"
	server_api DELETE "/sandboxes/$source_id" >/dev/null \
		|| log "verify cleanup: DELETE $source_id failed"
	wait_for "snapshot source sandbox deleted" 120 verify_sandbox_gone
	for snapshot_id in "$SNAPSHOT_ID" "$SNAPSHOT_ID2" "$SNAPSHOT_EXTRA"; do
		[[ -n "$snapshot_id" ]] || continue
		server_api DELETE "/snapshots/$snapshot_id" >/dev/null \
			|| log "verify cleanup: DELETE snapshot $snapshot_id failed"
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
	exec uv run pytest tests/test_fsb_e2e.py
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
	printf '  %-22s %s\n' "fast-sandbox" "pinned $(git -C "$FSB_DIR" rev-parse --short HEAD 2>/dev/null || echo "$FSB_COMMIT") ($FSB_DIR)"
	printf '  %-22s %s\n' "MinIO endpoint" "$MINIO_ENDPOINT"
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
usage: integration-env.sh [--auto-clean] {up|down|status|pool|sdk-e2e}

  up       initialize the full environment: fast-sandbox@pinned-commit images,
           two-node kind cluster (KVM), MinIO, control plane, firecracker
           firecracker runtime readiness + DART (P2P), SandboxTemplate golden
           image, firecracker-egress-pool (egress attached), the
           source-built OpenSandbox server + ingress gateway, and
           end-to-end verifies (create -> gateway route -> execd /ping,
           plus a pause/resume round-trip through the checkpoint).
  pool     re-apply only the SandboxPool (after editing manifests/pool/)
  status   nodes / pods / pool / DART P2P counters / MinIO / OpenSandbox health
  sdk-e2e  run the Python SDK e2e suite (tests/python/tests/test_fsb_e2e.py)
           against the live stack; requires `up` (template id at
           $WORK/template-id) and uv on PATH. Extra pytest args go through
           PYTEST_ADDOPTS.
  down     teardown: kind cluster + MinIO + sysctl + XFS StateRoot + caches

  --auto-clean  on up failure, run down automatically before dumping logs

Notable env overrides: WORK, FSB_DIR, KIND_CLUSTER, KIND_SINGLE,
DOCKER_MIRROR, MINIO_*, EGRESS_IMAGE, SERVER_IMAGE, INGRESS_IMAGE,
FSB_GOPROXY (default direct; set e.g. https://mirrors.aliyun.com/goproxy/,direct
when the host cannot reach module VCS hosts directly),
IMAGE_<COMPONENT>, POOL_MIN/POOL_MAX, WARM_IMAGES=1, SBX_IMAGE, EXECD,
XFS_STATEROOT=0, SKIP_TOOL_INSTALL=1, SKIP_LEFTOVER_CLEAN=1.
See the header of this script.
EOF
	exit 1
}

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
			echo "cluster=$KIND_CLUSTER single=$KIND_SINGLE minio=$MINIO_IMAGE port=$MINIO_PORT bucket=$MINIO_BUCKET"
			echo "sbxImage=$SBX_IMAGE execd=$EXECD warmImages=$WARM_IMAGES"
			echo "pool=$POOL_NAME poolMin=$POOL_MIN egress=$IMG_EGRESS"
			echo "server=$IMG_SERVER ingress=$IMG_INGRESS fastpath=$FASTPATH_ENDPOINT"
			echo "images: controller=$IMG_CONTROLLER runtime=$IMG_RUNTIME"
		} > "$LOGS_DIR/environment.txt" 2>&1 || true
		if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
			|| docker ps -a --format '{{.Names}}' | grep -qx "$MINIO_CONTAINER"; then
			if [[ "$SKIP_LEFTOVER_CLEAN" == 1 ]]; then
				log "leftover resources detected; aborting (SKIP_LEFTOVER_CLEAN=1). Run 'integration-env.sh down' first"
				exit 1
			fi
			log "leftover resources detected; cleaning and rebuilding"
			down
		fi
		trap 'on_error up' ERR
		run_stage "preflight + tooling" preflight
		run_stage "sysctl (fs.inotify)" sysctl_set
		run_stage "fast-sandbox checkout @ pinned commit" ensure_fsb
		run_stage "build images (fast-sandbox + OpenSandbox)" build_images
		run_stage "XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "render kind config" render_kind_config
		run_stage "kind cluster (KVM passthrough + labels)" kind_up
		run_stage "MinIO + bucket" minio_up
		run_stage "MinIO endpoint (kind network)" resolve_minio_endpoint
		run_stage "CRDs + control plane" control_plane_up
		run_stage "credentials (publish/pull)" credentials_up
		run_stage "firecracker runtime readiness + DART (P2P)" runtime_up
		run_stage "OpenSandbox server + ingress gateway" opensandbox_up
		run_stage "SandboxTemplate build (server API)" template_up
		run_stage "SandboxPool $POOL_NAME (egress + P2P)" pool_up
		run_stage "end-to-end verify (templateId create -> gateway -> execd /ping)" opensandbox_verify
		run_stage "pause/resume verify (server API -> FastPath checkpoint)" pause_resume_verify
		run_stage "snapshot verify (server API -> SandboxSnapshot -> restore)" snapshot_verify
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
		sdk_e2e
		;;
	down)
		down
		;;
esac
