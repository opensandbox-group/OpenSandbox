#!/usr/bin/env bash
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
# Read-only host check. No installations, sudo, directories, cluster or kernel changes.
set -uo pipefail
blockers=0
ok() { printf 'OK: %s\n' "$*"; }
block() { printf 'BLOCKER: %s\n' "$*"; blockers=$((blockers + 1)); }
printf 'DeepSeek Harness Ubuntu preflight (read-only; no host changes)\n'
[[ "$(uname -s)" == Linux ]] && ok Linux || block 'Linux is required'
[[ "$(uname -m)" == x86_64 ]] && ok x86_64 || block 'This official environment guide targets x86_64'
if [[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm ]]; then ok '/dev/kvm exists and is readable/writable by this user';
else block '/dev/kvm is missing or this user lacks read/write access'; fi

for tool in docker go git curl jq make gcc kind kubectl helm uv python3 node ss mkfs.xfs findmnt losetup truncate openssl realpath df awk sed stat grep find; do
  command -v "$tool" >/dev/null 2>&1 && ok "tool $tool" || block "required tool unavailable: $tool"
done
if command -v go >/dev/null 2>&1; then
  version="$(GOTOOLCHAIN=local go version 2>/dev/null || true)"
  if [[ "$version" =~ go([0-9]+)\.([0-9]+) ]] && (( BASH_REMATCH[1] > 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 25) )); then ok 'Go >=1.25';
  else block 'Go >=1.25 is required (no automatic toolchain download attempted)'; fi
fi
if command -v node >/dev/null 2>&1; then
  version="$(node --version 2>/dev/null || true)"
  if [[ "$version" =~ ^v([0-9]+)\.([0-9]+) ]] && (( BASH_REMATCH[1] >= 24 || (BASH_REMATCH[1] == 22 && BASH_REMATCH[2] >= 19) )); then ok 'Node engine range';
  else block 'Node ^22.19.0 or >=24.0.0 is required'; fi
fi
# Inspect the resolved target and package identity BEFORE executing pnpm. Corepack and
# other unresolved launch/download shims are not invoked, even with network disabled.
pnpm_target="${DSH_PNPM_EXECUTABLE:-$(command -v pnpm 2>/dev/null || true)}"
if command -v realpath >/dev/null 2>&1; then pnpm_target="$(realpath -e -- "$pnpm_target" 2>/dev/null || true)"; else pnpm_target=""; fi
case "$pnpm_target" in
  */pnpm/bin/pnpm.cjs|*/pnpm/bin/pnpm.mjs)
    pnpm_package="${pnpm_target%/bin/*}/package.json"
    if [[ -f "$pnpm_target" && -r "$pnpm_target" && -f "$pnpm_package" ]] && grep -Eq '"name"[[:space:]]*:[[:space:]]*"pnpm"' "$pnpm_package"; then
      version="$(npm_config_manage_package_manager_versions=false node "$pnpm_target" --version 2>/dev/null || true)"
      [[ "$version" == 11.7.0 ]] && ok 'pnpm 11.7.0 (identified installed executable)' || block 'Install/use pnpm 11.7.0 explicitly before package checks'
    else block 'unresolved pnpm package identity; do not invoke a version-manager shim'; fi
    ;;
  *) block 'unresolved pnpm executable or download shim; set DSH_PNPM_EXECUTABLE to an already installed pnpm/bin/pnpm.cjs or pnpm.mjs';;
esac
if [[ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null || true)" == cgroup2fs ]]; then ok 'host cgroup v2'; else block 'host cgroup v2 is required'; fi
if command -v docker >/dev/null 2>&1; then
  if docker info >/dev/null 2>&1; then
    ok 'Docker daemon reachable'
    [[ "$(docker info --format '{{.CgroupVersion}}' 2>/dev/null)" == 2 ]] && ok 'Docker cgroup v2' || block 'Docker must report cgroup version 2'
    docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
  else block 'Docker daemon is not reachable by this user'; fi
fi

root="${DSH_WORKSPACE_ROOT:-}"; work="${WORK:-}"; evidence="${DSH_EVIDENCE_PATH:-}"; downloads="${DSH_DOWNLOADS_DIR:-}"
for name in DSH_WORKSPACE_ROOT WORK DSH_EVIDENCE_PATH DSH_DOWNLOADS_DIR; do
  value="${!name:-}"
  [[ "$value" == /* && "$value" != / ]] && ok "$name is an absolute non-root path" || block "$name must be an explicit absolute non-root path"
done
# These are safety-sensitive inputs to the actual pinned official script.
# Its omitted defaults include /var/lib/fast-sandbox and shared resource names.
for name in FSB_DIR XFS_LOOP_FILE XFS_MOUNT_POINT KIND_CLUSTER RUSTFS_CONTAINER; do
  [[ -n "${!name:-}" ]] || block "$name must be explicit; no alternate safe default is assumed"
done
if [[ "${KIND_CLUSTER:-}" =~ ^[a-z0-9.-]+$ ]]; then ok 'kind cluster name syntax';
else block 'KIND_CLUSTER must match ^[a-z0-9.-]+$'; fi
if command -v realpath >/dev/null 2>&1 && [[ "$root" == /* && "$work" == /* && "$evidence" == /* && "$downloads" == /* ]]; then
  root="$(realpath -m -- "$root")"; work="$(realpath -m -- "$work")"; evidence="$(realpath -m -- "$evidence")"; downloads="$(realpath -m -- "$downloads")"
  [[ -d "$root" && -w "$root" ]] && ok 'workspace root exists and is writable' || block 'workspace root must already exist and be writable'
  [[ "$work" == "$root/env" ]] && ok 'WORK is the dedicated workspace env path' || block 'WORK must be DSH_WORKSPACE_ROOT/env'
  [[ ! -e "$work" && ! -L "${WORK:-}" ]] && ok 'WORK is unused' || block 'WORK already exists; stop and review the prior run, do not auto-clean/reuse'
  [[ "$evidence" == "$root/evidence/"* && "$downloads" == "$root/downloads" ]] && ok 'evidence/downloads outside teardown WORK' || block 'use dedicated evidence/ and downloads/ beside env/'
  [[ -d "$(dirname -- "$evidence")" && -w "$(dirname -- "$evidence")" ]] || block 'evidence parent must exist and be writable'
  [[ ! -e "$evidence" && ! -L "${DSH_EVIDENCE_PATH:-}" ]] || block 'evidence file already exists; choose a new evidence filename'
  [[ -d "$downloads" && -w "$downloads" ]] || block 'downloads directory must exist and be writable'
  if download_entry="$(find "$downloads" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)"; then
    [[ -z "$download_entry" ]] || block 'downloads directory is not empty; retain artifacts and choose a fresh run workspace'
  else block 'cannot inspect the downloads directory'; fi
  for name in FSB_DIR XFS_LOOP_FILE XFS_MOUNT_POINT; do
    value="${!name:-}"
    [[ "$value" == /* ]] || { block "$name must be an explicit absolute path"; continue; }
    canonical="$(realpath -m -- "$value")"
    [[ "$canonical" == "$work/"* ]] || block "$name must stay inside dedicated WORK"
    [[ ! -e "$value" && ! -L "$value" ]] || block "$name already exists; stop before potentially destructive official up/down"
  done
  # df is run against the nearest existing ancestor; no directories are created.
  for target in "$work" "$downloads" "${docker_root:-$root}"; do
    while [[ ! -e "$target" && "$target" != / ]]; do target="$(dirname -- "$target")"; done
    available="$(df -Pk -- "$target" 2>/dev/null | awk 'NR==2 {print $4}')"
    if [[ "$available" =~ ^[0-9]+$ ]] && (( available >= 20 * 1024 * 1024 )); then ok 'disk headroom >=20 GiB on a target filesystem';
    else block 'at least 20 GiB free is required on each WORK/download/Docker filesystem'; fi
  done
fi
if command -v ss >/dev/null 2>&1; then
  for port in "${SERVER_HOST_PORT:-18080}" "${GATEWAY_HOST_PORT:-18081}" "${RUSTFS_PORT:-19000}" "${RUSTFS_CONSOLE_PORT:-19001}"; do
    if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then block 'invalid configured TCP port';
    elif ! listeners="$(ss -H -ltn "sport = :$port" 2>/dev/null)"; then block "cannot inspect TCP listeners for port $port";
    elif [[ -n "$listeners" ]]; then block "TCP port $port already listens";
    else ok "TCP port $port is unused"; fi
  done
fi
if command -v kind >/dev/null 2>&1; then
  if clusters="$(kind get clusters 2>/dev/null)"; then
    [[ "$clusters" != *"${KIND_CLUSTER:-fast-sandbox-integration}"* ]] || block 'named kind cluster already exists; review its ownership before any teardown'
  else block 'cannot inspect kind clusters'; fi
fi
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  if containers="$(docker ps -a --format '{{.Names}}' 2>/dev/null)"; then
    [[ "$containers" != *"${RUSTFS_CONTAINER:-fast-sandbox-env-rustfs}"* ]] || block 'named RustFS container already exists; review its ownership'
  else block 'cannot inspect Docker containers'; fi
fi
printf 'read-only preflight finished: %s blocker(s). No changes were made.\n' "$blockers"
(( blockers == 0 ))
