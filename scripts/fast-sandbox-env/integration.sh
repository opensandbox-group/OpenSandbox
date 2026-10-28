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

# integration.sh — one-command fast-sandbox integration environment,
# driven from the OpenSandbox repository.
#
# On a bare-metal Linux KVM host it builds the full stack — fast-sandbox at
# the commit pinned in manifests/third-party/fast-sandbox.commit, a two-node
# kind cluster with KVM passthrough, the OpenSandbox Helm charts (base CRDs,
# fast-sandbox control plane, server, ingress-gateway), a RustFS artifact
# store, and the firecracker-egress-pool SandboxPool with the OpenSandbox
# egress sidecar — then verifies end to end: sandbox create via the
# opensandbox CLI, execd /ping through the signed gateway route, delete,
# a pause/resume round trip, a credential-vault relay lifecycle, the
# OSEP-0024 idle-pause/wake profile (IDLE_PAUSE=1), and a snapshot ->
# restore-from-snapshotId boot.
#
# All Kubernetes resources come from the charts via `helm template` + plain
# `kubectl apply` (no helm release state); the only env-owned manifests are
# the kind cluster config and the SandboxPool. The verify stages drive the
# server through the published opensandbox CLI ("osb", installed fresh from
# PyPI; OSB_BIN overrides); raw curl remains only for the calls the CLI
# cannot express (networkpolicy replace PUT, metadata merge-patch, snapshot
# re-entry fence) plus the gateway /ping probes.
#
# The script is split for maintainability; this file is the entry that
# sources and chains the parts (a single process, so the ERR trap /
# failure-dump / --auto-clean contract and the unified stage summary keep
# working across all stages):
#
#   lib.sh               shared config, logging/stage machinery, failure dump,
#                        tooling, server/CLI helpers (sourced by every part)
#   env.sh               environment lifecycle: env_up_stages (preflight ->
#                        SandboxPool), down, status, pool
#   verify/sandbox.sh    end-to-end sandbox verify + egress policy + lifecycle ops
#   verify/pause-resume.sh  pause/resume round trip
#   verify/vault.sh      credential-vault relay lifecycle (OSEP-0022)
#   verify/idle-pause.sh idle-pause / wake-on-access (OSEP-0024, IDLE_PAUSE=1)
#   verify/snapshot.sh   snapshot -> restore round trip
#
# env.sh and each verify script also run standalone against a live stack.
#
# Usage:
#   ./scripts/fast-sandbox-env/integration.sh up       # full environment + pool + server/ingress + verify
#   ./scripts/fast-sandbox-env/integration.sh pool     # re-apply the pool only
#   ./scripts/fast-sandbox-env/integration.sh status   # component/pool/DART/OpenSandbox health
#   ./scripts/fast-sandbox-env/integration.sh down     # teardown, host left clean
#   ./scripts/fast-sandbox-env/integration.sh up --auto-clean   # down on failure
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
#   IDLE_PAUSE_ROUNDS  auto-pause/wake rounds in the idle-pause verify stage (default 3)
#   IDLE_PAUSE_PARK_BUDGET  gateway wake park budget (default 5s; set e.g. 100ms to
#                    force every wake through the marked-503 + client-retry path)
#   REDIS_IMAGE      Redis image for the idle-pause profile (default redis:7-alpine)
#
# Every stage logs to $WORK/logs/; failures dump component logs to
# logs/failure-<task>-<ts>.txt before exiting (never silently).

# Everything below sources into this shell: one process for the whole `up`,
# so stage timings, the ERR trap and --auto-clean stay unified.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
source "$SCRIPT_DIR/env.sh"
source "$SCRIPT_DIR/verify/sandbox.sh"
source "$SCRIPT_DIR/verify/pause-resume.sh"
source "$SCRIPT_DIR/verify/vault.sh"
source "$SCRIPT_DIR/verify/idle-pause.sh"
source "$SCRIPT_DIR/verify/snapshot.sh"


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
	template_id="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$template_id" ]] || die "no template id at $WORK/template-id (run up first; template_up stores it there)"
	export OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$template_id"
	export OPENSANDBOX_TEST_DOMAIN="127.0.0.1:$SERVER_HOST_PORT"
	export OPENSANDBOX_TEST_PROTOCOL="http"
	export OPENSANDBOX_TEST_API_KEY="$SERVER_API_KEY"
	export OPENSANDBOX_TEST_IDLE_PAUSE="1"
	log "sdk e2e (idle-pause): template=$template_id server=$SERVER_URL -> tests/python/tests/test_fsb_idle_pause_e2e.py"
	cd "$OSB_ROOT/tests/python" || die "tests/python not found under $OSB_ROOT"
	uv run pytest tests/test_fsb_idle_pause_e2e.py
}


usage() {
	cat <<'EOUT'
usage: integration.sh [--auto-clean] {up|down|status|pool|sdk-e2e|idle-e2e}

  up       initialize the full environment and run every verify stage:
           fast-sandbox@pinned-commit images, two-node kind cluster (KVM),
           RustFS, control plane, firecracker runtime readiness + DART (P2P),
           SandboxTemplate golden image, firecracker-egress-pool (egress
           attached), the source-built OpenSandbox server + ingress gateway,
           then the verify stages (see verify/): end-to-end create ->
           gateway route -> execd /ping, pause/resume, credential-vault
           relay lifecycle, idle-pause/wake (IDLE_PAUSE=1) and the snapshot
           -> restore round trip. The CLI (osb) is installed fresh from
           PyPI unless OSB_BIN points at an existing binary. With
           IDLE_PAUSE=1: Redis + server [automation.idle_pause] + gateway
           activity/wake flags (IDLE_PAUSE_ROUNDS cycles, per-round timing
           summary).
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

  Environment pieces can also be driven directly: env.sh
  {up|down|status|pool} (provisioning without verifies) and the
  verify/*.sh scripts (one verify stage each, against a live stack).

  The RustFS migration replaces MINIO_* and MC_IMAGE overrides. Before
  upgrading an existing environment, run `down` with the previous script.

Notable env overrides: WORK, FSB_DIR, KIND_CLUSTER, KIND_SINGLE,
DOCKER_MIRROR, RUSTFS_*, RC_IMAGE, EGRESS_IMAGE, SERVER_IMAGE, INGRESS_IMAGE,
FSB_GOPROXY (default direct; set e.g. https://mirrors.aliyun.com/goproxy/,direct
when the host cannot reach module VCS hosts directly),
IMAGE_<COMPONENT>, POOL_MIN/POOL_MAX, WARM_IMAGES=1, SBX_IMAGE, EXECD,
XFS_STATEROOT=0, OSB_PACKAGE/OSB_BIN, SKIP_TOOL_INSTALL=1, SKIP_LEFTOVER_CLEAN=1,
IDLE_PAUSE=1 (OSEP-0024 idle-pause/wake profile; REDIS_IMAGE overrides the
Redis image). See the header of this script.
EOUT
	exit 1
}

# Allow focused tests to load the stage functions without starting the environment.
[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0

for arg in "$@"; do
	case "$arg" in
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|pool|sdk-e2e|idle-e2e) ACTION="$arg" ;;
		*) usage ;;
	esac
done
[[ -n "$ACTION" ]] || usage

case "$ACTION" in
	up)
		env_up_prelude
		trap 'on_error up' ERR
		env_up_stages
		run_stage "end-to-end verify (CLI create -> gateway -> execd /ping)" opensandbox_verify
		run_stage "pause/resume verify (CLI -> FastPath checkpoint)" pause_resume_verify
		run_stage "vault lifecycle verify (CLI -> relay -> egress-held vault)" vault_lifecycle_verify
		run_stage "idle-pause / wake-on-access verify (OSEP-0024 profile)" idle_pause_verify
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
	idle-e2e)
		kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null \
			|| die "cluster $KIND_CLUSTER is not up (run up first)"
		trap 'on_error idle-e2e' ERR
		idle_e2e
		trap - ERR
		;;
	down)
		down
		;;
esac
