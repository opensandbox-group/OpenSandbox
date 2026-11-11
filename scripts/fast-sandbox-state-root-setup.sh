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

# Provision a new loop-backed XFS state disk. Never reuse or format existing data.
set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
usage() {
    cat <<'EOF'
Usage: fast-sandbox-state-root-setup.sh [--dry-run] [SIZE [MOUNTPOINT [BACKING_FILE]]]
  SIZE          integer GiB, e.g. 50G (default: 50G; minimum: 11G)
  MOUNTPOINT    default: /var/lib/fast-sandbox/firecracker
  BACKING_FILE  default: /var/lib/fast-sandbox/state-root.xfs
Run before deploying the runtime. Requires root, xfsprogs, coreutils and util-linux.
--dry-run checks inputs and space without writing anything (root not required).
Existing files, symlinked paths, and mounted/nonempty targets are rejected.
The file is preallocated, not sparse. The fstab entry is printed, not installed.
EOF
}

dry_run=0
case "${1:-}" in
    -h|--help) usage; exit 0 ;;
    --dry-run) dry_run=1; shift ;;
esac
(( $# <= 3 )) || die "too many arguments (see --help)"
size=${1:-50G}
target=${2:-/var/lib/fast-sandbox/firecracker}
backing=${3:-/var/lib/fast-sandbox/state-root.xfs}
[[ $size =~ ^[1-9][0-9]{0,5}G$ ]] || die "size must be an integer GiB value such as 50G"
gib=${size%G}
(( gib >= 11 )) || die "size must exceed the 10 GiB readiness floor (minimum 11G)"
bytes=$((gib * 1024 * 1024 * 1024))
reserve=$((10 * 1024 * 1024 * 1024))

for tool in realpath df awk find mountpoint; do
    command -v "$tool" >/dev/null || die "missing dependency: $tool"
done
for path in "$target" "$backing"; do
    [[ $path =~ ^/[a-zA-Z0-9_./-]+$ ]] || die "paths must be absolute and contain no whitespace or shell metacharacters"
    [[ $(realpath -m -- "$path") == $(realpath -ms -- "$path") ]] || die "symlinked paths are not supported: $path"
done
target=$(realpath -m -- "$target")
backing=$(realpath -m -- "$backing")
[[ $target != / && $backing != "$target" && $backing != "$target/"* ]] || die "backing file must be outside the mountpoint"
[[ ! -e $backing && ! -L $backing ]] || die "backing file already exists: $backing"
mountpoint -q -- "$target" && die "target is already mounted: $target"
if [[ -e $target ]]; then
    [[ -d $target ]] || die "target is not a directory"
    [[ -z $(find "$target" -mindepth 1 -maxdepth 1 -print -quit) ]] || die "target is not empty: $target"
fi
parent=${backing%/*}
[[ -n $parent ]] || parent=/
ancestor=$parent
while [[ ! -e $ancestor ]]; do ancestor=${ancestor%/*}; [[ -n $ancestor ]] || ancestor=/; done
[[ -d $ancestor ]] || die "backing parent is not a directory"
available=$(df -B1 --output=avail -- "$ancestor" | awk 'NR==2 {print $1}')
[[ $available =~ ^[0-9]+$ ]] || die "could not determine available space"
(( available >= bytes + reserve )) || die "insufficient space: need ${size} plus 10 GiB free on $ancestor"

printf 'Plan: allocate %s at %s; mount XFS reflink at %s\n' "$size" "$backing" "$target"
if (( dry_run )); then exit 0; fi
(( EUID == 0 )) || die "run as root (or use --dry-run)"
for tool in fallocate mkfs.xfs mount cp mktemp rmdir rm flock; do
    command -v "$tool" >/dev/null || die "missing dependency: $tool"
done
umask 077
mkdir -p -- "$parent" "$target"
# Serialize helpers using the same backing directory; noclobber also prevents reuse.
exec 9>>"$parent/.fast-sandbox-state-root.lock"
flock -n 9 || die "another setup is running in $parent"
mountpoint -q -- "$target" && die "target is already mounted: $target"
[[ -z $(find "$target" -mindepth 1 -maxdepth 1 -print -quit) ]] || die "target is not empty: $target"
(set -o noclobber; : > "$backing") || die "refusing to overwrite $backing"
probe=
cleanup() {
    status=$?
    if [[ -n $probe ]]; then
        rm -f -- "$probe/source" "$probe/clone"
        rmdir -- "$probe"
    fi
    if (( status != 0 )); then
        echo "Setup failed; inspect $backing and mounts before retrying. Created data was retained." >&2
    fi
}
trap cleanup EXIT
fallocate -l "$bytes" -- "$backing"
mkfs.xfs -m reflink=1 "$backing"
mount -o loop,noatime -- "$backing" "$target"
probe=$(mktemp -d "$target/.reflink-check.XXXXXX")
printf 'reflink verification\n' > "$probe/source"
cp --reflink=always -- "$probe/source" "$probe/clone"
echo 'OK: XFS mounted; reflink copy verified.'
echo 'For persistence, review and add this entry to /etc/fstab:'
printf '%s %s xfs loop,noatime 0 0\n' "$backing" "$target"
echo 'Mount before deploying the runtime; otherwise drain workloads and restart the runtime Pods.'
