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

# build-fast-sandbox.sh — build the fast-sandbox companion images from the
# source pinned in manifests/third-party/fast-sandbox.commit.
#
# The pin file (Git-LFS-pointer style) names the exact upstream commit this
# repository builds against. The script materializes a checkout of that
# commit (cached under .fast-sandbox/src, safe to delete) and builds the
# Firecracker-scope image set only:
#
#   controller, fastlet, fastlet-proxy, janitor,
#   firecracker-runtime-agent, sandboxtemplate-builder
#
# boxlite-runtime and sandbox-action-fixture are intentionally NOT built.
# sandbox-proxy is not built either: OpenSandbox deployments reach fastlets
# through the ingress gateway's direct route resolution.
#
# With --sync-crds it also re-vendors the fast-sandbox CRDs from the pinned
# checkout into manifests/charts/base/files/fast-sandbox-crds.yaml, keeping
# the Helm bundle byte-identical to the pinned source of truth.
#
# Usage:
#   manifests/release/build-fast-sandbox.sh                        # build all images
#   manifests/release/build-fast-sandbox.sh --sync-crds            # build + sync CRDs
#   manifests/release/build-fast-sandbox.sh --no-build --sync-crds # sync CRDs only
#   manifests/release/build-fast-sandbox.sh --load-kind <cluster>  # build + kind load
#   manifests/release/build-fast-sandbox.sh --push                 # build + push to the default registries
#   manifests/release/build-fast-sandbox.sh --push r1,r2           # build + push to the listed registries
#   manifests/release/build-fast-sandbox.sh --list-images          # print image refs
#
# Push follows the components/* build.sh convention (retag + docker push of
# the locally built images): bare --push targets docker.io/opensandbox and
# sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox (plus
# $GHCR_REPO/<component> when GHCR_REPO is set); --push r1[,r2...] targets
# exactly the listed registries. A v* TAG additionally pushes :latest.
# Images are linux/amd64 only (the fast-sandbox Makefile hardcodes GOARCH).
#
# Environment overrides:
#   PIN_FILE     pin file path      (default manifests/third-party/fast-sandbox.commit)
#   FSB_SRC_DIR  checkout location  (default <repo>/.fast-sandbox/src)
#   REGISTRY     image registry     (default fast-sandbox)
#   TAG          image tag          (default dev)
#   GHCR_REPO    extra push registry (only with bare --push, mirrors components/*)
#   DOCKER_BUILD_FLAGS  extra docker build flags (e.g. --platform linux/amd64)

set -euo pipefail

OSB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PIN_FILE="${PIN_FILE:-$OSB_ROOT/manifests/third-party/fast-sandbox.commit}"
FSB_SRC_DIR="${FSB_SRC_DIR:-$OSB_ROOT/.fast-sandbox/src}"
# fast-sandbox images publish under the unified opensandbox/ namespace with
# an fsb- prefix: it disambiguates fsb-controller from the k8s controller
# image (opensandbox/controller) and inherits the components/* registry
# mirror set (DockerHub + ACR + GHCR).
REGISTRY="${REGISTRY:-opensandbox}"
TAG="${TAG:-dev}"

CRDS_TARGET="$OSB_ROOT/manifests/charts/base/files/fast-sandbox-crds.yaml"

log() { printf '\033[1;34m[build-fast-sandbox]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[build-fast-sandbox] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage:
  manifests/release/build-fast-sandbox.sh                        # build all images
  manifests/release/build-fast-sandbox.sh --sync-crds            # build + sync CRDs
  manifests/release/build-fast-sandbox.sh --no-build --sync-crds # sync CRDs only
  manifests/release/build-fast-sandbox.sh --load-kind <cluster>  # build + kind load
  manifests/release/build-fast-sandbox.sh --push                 # build + push to the default registries
  manifests/release/build-fast-sandbox.sh --push r1,r2           # build + push to the listed registries
  manifests/release/build-fast-sandbox.sh --list-images          # print image refs

Push (--push, same convention as components/* build.sh):
  bare --push  -> docker.io/opensandbox + sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox
                  (+ $GHCR_REPO when set)
  --push r1,r2 -> exactly the listed registries (comma-separated, repeatable)
  a v* TAG also pushes :latest; images are linux/amd64 only

Environment overrides:
  PIN_FILE     pin file path      (default manifests/third-party/fast-sandbox.commit)
  FSB_SRC_DIR  checkout location  (default <repo>/.fast-sandbox/src)
  REGISTRY     image registry     (default fast-sandbox)
  TAG          image tag          (default dev)
  GHCR_REPO    extra push registry (only with bare --push, mirrors components/*)
  DOCKER_BUILD_FLAGS  extra docker build flags (e.g. --platform linux/amd64)
EOF
	exit 0
}

SYNC_CRDS=0
NO_BUILD=0
KIND_CLUSTER=""
LIST_IMAGES=0
PUSH_MODE=0
PUSH_REGISTRIES=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	--sync-crds) SYNC_CRDS=1 ;;
	--no-build) NO_BUILD=1 ;;
	--load-kind)
		[[ $# -ge 2 ]] || die "--load-kind requires a kind cluster name"
		KIND_CLUSTER="$2"
		shift
		;;
	--push)
		PUSH_MODE=1
		# Optional registry list: bare --push uses the components/* default
		# registry set; --push r1[,r2...] targets exactly those registries.
		if [[ $# -ge 2 && "$2" != -* ]]; then
			IFS=',' read -ra regs <<<"$2"
			PUSH_REGISTRIES+=("${regs[@]}")
			shift
		fi
		;;
	--list-images) LIST_IMAGES=1 ;;
	-h | --help) usage ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
	shift
done

# --- pin --------------------------------------------------------------------------------------

[[ -f "$PIN_FILE" ]] || die "pin file not found: $PIN_FILE"
FSB_REPO="$(sed -n 's/^repo:[[:space:]]*//p' "$PIN_FILE")"
FSB_COMMIT="$(sed -n 's/^commit:[[:space:]]*//p' "$PIN_FILE")"
[[ -n "$FSB_REPO" ]] || die "pin file $PIN_FILE carries no repo"
[[ "$FSB_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
	die "pin file $PIN_FILE must carry a full 40-character commit SHA (got: ${FSB_COMMIT:-<empty>})"

# image-name:make-COMPONENT:make-image-variable — the exact Firecracker scope.
# sandboxtemplate-builder has no make target (direct docker build, same as
# scripts/fast-sandbox-env).
IMAGES=(
	"fsb-controller:controller:CONTROLLER_IMAGE"
	"fsb-fastlet:fastlet:FASTLET_IMAGE"
	"fsb-fastlet-proxy:fastlet-proxy:FASTLET_PROXY_IMAGE"
	"fsb-janitor:janitor:JANITOR_IMAGE"
	"fsb-firecracker-runtime:firecracker-runtime:FIRECRACKER_RUNTIME_IMAGE"
	"fsb-sandboxtemplate-builder:sandboxtemplate-builder:SANDBOXTEMPLATE_BUILDER_IMAGE"
)

image_ref() { printf '%s/%s:%s' "$REGISTRY" "$1" "$TAG"; }

if [[ "$LIST_IMAGES" == 1 ]]; then
	for entry in "${IMAGES[@]}"; do image_ref "${entry%%:*}"; printf '\n'; done
	exit 0
fi

# --- checkout ---------------------------------------------------------------------------------

if [[ ! -d "$FSB_SRC_DIR/.git" ]]; then
	log "cloning $FSB_REPO into $FSB_SRC_DIR"
	mkdir -p "$(dirname "$FSB_SRC_DIR")"
	# Partial clone keeps the cache small; fall back for servers without
	# filter support.
	git clone --filter=blob:none "$FSB_REPO" "$FSB_SRC_DIR" 2>/dev/null ||
		git clone "$FSB_REPO" "$FSB_SRC_DIR" ||
		die "clone failed; check network / FSB_REPO=$FSB_REPO"
else
	# Compare the raw configured URL (git remote get-url applies
	# url.insteadOf rewrites, which would re-trigger the repoint every run).
	orig="$(git -C "$FSB_SRC_DIR" config --get remote.origin.url || true)"
	if [[ "$orig" != "$FSB_REPO" ]]; then
		log "repointing checkout origin: $orig -> $FSB_REPO"
		git -C "$FSB_SRC_DIR" remote set-url origin "$FSB_REPO"
	fi
fi

# A raw SHA fetch needs allow-reachable-sha1-in-want (GitHub supports it);
# fall back to a full ref fetch for other remotes.
pinned_head=""
if git -C "$FSB_SRC_DIR" fetch --quiet origin "$FSB_COMMIT" 2>/dev/null; then
	pinned_head="$(git -C "$FSB_SRC_DIR" rev-parse FETCH_HEAD)"
else
	log "SHA fetch unsupported by remote; fetching all refs"
	git -C "$FSB_SRC_DIR" fetch --quiet origin '+refs/heads/*:refs/remotes/origin/*' ||
		die "git fetch failed for $FSB_REPO"
	pinned_head="$(git -C "$FSB_SRC_DIR" rev-parse --verify --quiet "$FSB_COMMIT^{commit}" || true)"
fi
[[ "$pinned_head" == "$FSB_COMMIT" ]] || die "pinned commit $FSB_COMMIT is not reachable from $FSB_REPO"

current_head="$(git -C "$FSB_SRC_DIR" rev-parse HEAD 2>/dev/null || true)"
if [[ "$current_head" != "$FSB_COMMIT" ]]; then
	# New pinned commit: drop untracked/ignored build leftovers so the tree
	# is exactly the pinned source (incremental rebuilds on the same commit
	# keep their caches).
	git -C "$FSB_SRC_DIR" clean -ffdx
fi
git -C "$FSB_SRC_DIR" checkout --force --quiet "$FSB_COMMIT" ||
	die "git checkout $FSB_COMMIT failed"
actual="$(git -C "$FSB_SRC_DIR" rev-parse HEAD)"
[[ "$actual" == "$FSB_COMMIT" ]] || die "checkout at $actual, expected $FSB_COMMIT"
log "fast-sandbox checkout ready @ $(git -C "$FSB_SRC_DIR" rev-parse --short HEAD)"

# --- build ------------------------------------------------------------------------------------

build_images() {
	local entry image component var
	for entry in "${IMAGES[@]}"; do
		image="$REGISTRY/${entry%%:*}:$TAG"
		component="$(cut -d: -f2 <<<"$entry")"
		log "building $image"
		if [[ "$component" == "sandboxtemplate-builder" ]]; then
			# shellcheck disable=SC2086
			docker build ${DOCKER_BUILD_FLAGS:-} --quiet \
				-t "$image" -f "$FSB_SRC_DIR/build/Dockerfile.sandboxtemplate-builder" \
				"$FSB_SRC_DIR" >/dev/null || die "sandboxtemplate-builder image build failed"
		else
			var="$(cut -d: -f3 <<<"$entry")"
			(make -C "$FSB_SRC_DIR" images "COMPONENT=$component" \
				"REGISTRY=$REGISTRY" "$var=$image" >/dev/null) ||
				die "make images COMPONENT=$component failed"
		fi
	done
	log "images built (firecracker scope)"
}

load_kind() {
	local entry
	for entry in "${IMAGES[@]}"; do
		kind load docker-image "$(image_ref "${entry%%:*}")" --name "$KIND_CLUSTER" >/dev/null
	done
	log "images loaded into kind cluster $KIND_CLUSTER"
}

push_images() {
	# Retag + push the locally built images (no rebuild). Default registry
	# set mirrors components/*/build.sh; a v* TAG also pushes :latest.
	local entry comp ref reg tag target
	local tags=("$TAG")
	if [[ "$TAG" == v* ]]; then
		tags+=("latest")
	fi
	for entry in "${IMAGES[@]}"; do
		comp="${entry%%:*}"
		ref="$(image_ref "$comp")"
		for reg in "${PUSH_REGISTRIES[@]}"; do
			for tag in "${tags[@]}"; do
				target="$reg/$comp:$tag"
				docker tag "$ref" "$target" || die "docker tag $target failed"
				log "pushing $target"
				docker push "$target" >/dev/null || die "docker push $target failed"
			done
		done
	done
	log "images pushed: ${PUSH_REGISTRIES[*]}"
}

# --- CRD sync ---------------------------------------------------------------------------------

sync_crds() {
	# Bundle the CRDs in the order declared by config/crd/kustomization.yaml,
	# formatted exactly like `make helm-gen-crds` (leading '---' stripped,
	# documents joined with a trailing '---' separator).
	local tmp resource file
	tmp="$(mktemp)"
	for resource in $(awk '/^- /{sub(/^- /, ""); print}' "$FSB_SRC_DIR/config/crd/kustomization.yaml"); do
		file="$FSB_SRC_DIR/config/crd/$resource"
		[[ -f "$file" ]] || die "CRD manifest missing in pinned checkout: $file"
		awk 'NR==1 && $0=="---" {next} {print}' "$file" >>"$tmp"
		printf '\n---\n' >>"$tmp"
	done
	mkdir -p "$(dirname "$CRDS_TARGET")"
	mv "$tmp" "$CRDS_TARGET"
	log "CRDs synced from pinned commit -> ${CRDS_TARGET#$OSB_ROOT/}"
}

if [[ "$SYNC_CRDS" == 1 ]]; then
	sync_crds
fi
if [[ "$NO_BUILD" != 1 ]]; then
	command -v docker >/dev/null 2>&1 || die "docker not found in PATH"
	build_images
fi
if [[ -n "$KIND_CLUSTER" ]]; then
	command -v kind >/dev/null 2>&1 || die "kind not found in PATH"
	load_kind
fi
if [[ "$PUSH_MODE" == 1 ]]; then
	if [[ ${#PUSH_REGISTRIES[@]} -eq 0 ]]; then
		# components/*/build.sh default registry set + optional GHCR mirror.
		PUSH_REGISTRIES=(
			"opensandbox"
			"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox"
		)
		if [[ -n "${GHCR_REPO:-}" ]]; then
			PUSH_REGISTRIES+=("$GHCR_REPO")
		fi
	fi
	command -v docker >/dev/null 2>&1 || die "docker not found in PATH"
	push_images
fi
