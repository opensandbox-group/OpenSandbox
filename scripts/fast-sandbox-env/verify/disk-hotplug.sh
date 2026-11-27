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

# Standalone native-template KVM probe for #2941. Run after integration-env.sh up.
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

require_live_stack
require_template_id
for tool in mkfs.ext4 debugfs; do
	command -v "$tool" >/dev/null || die "install e2fsprogs on the KVM runner ($tool missing)"
done

tmp=$(mktemp -d)
cleanup() {
	local rc=$?
	trap - EXIT
	if (( rc != 0 )) && [[ -f "$tmp/stream" ]]; then
		tail -n 8 "$tmp/stream" >&2
	fi
	if [[ -n "$VERIFY_ID" ]]; then
		server_api DELETE "/sandboxes/$VERIFY_ID" >/dev/null || rc=1
	fi
	if [[ -n "${stream_pid:-}" ]]; then
		kill "$stream_pid" 2>/dev/null || true
		wait "$stream_pid" 2>/dev/null || true
	fi
	# The backing file belongs to the disposable VM jail; the runtime tears
	# it down with the sandbox, including on a partial hotplug failure.
	rm -r -- "$tmp"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

created=$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 15m -o json)
VERIFY_ID=$(jq -er '.id | strings | select(length > 0)' <<<"$created")
wait_for "template VM serving execd" 60 execd_ping_ok
uid=$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o jsonpath='{.metadata.uid}')
[[ "$uid" =~ ^[0-9a-f-]{36}$ ]] || die "unexpected Kubernetes sandbox UID"
# The two kind nodes have private StateRoots on the runner. Match the full
# discovery record before touching a jail whose directory uses a truncated UID.
jails=()
for role in control-plane worker; do
	root="${XFS_MOUNT_POINT:-/var/lib/fast-sandbox}/$role/firecracker/jails/firecracker/${uid:0:32}/root"
	if [[ -S "$root/api.sock" ]] && jq -e --arg uid "$uid" --arg id "$VERIFY_ID" --arg ns "$RESOURCE_NS" \
		'.uid == $uid and .name == $id and .namespace == $ns' "$root/fast-sandbox.json" >/dev/null; then
		jails+=("$root")
	fi
done
(( ${#jails[@]} == 1 )) || die "expected exactly one live jail for $RESOURCE_NS/$VERIFY_ID ($uid)"
jail=${jails[0]}

guest() { osb_api command run "$VERIFY_ID" -- "$@"; }
guest sh -ceu '
	echo "Restored kernel arguments: $(cat /proc/cmdline)"
	case " $(cat /proc/cmdline) " in
		*" pci=off "*) echo "Template disabled guest PCI. Rebuild it with PCI enabled." >&2; exit 1 ;;
	esac
	test -w /sys/bus/pci/rescan || { echo "guest PCI rescan unavailable" >&2; exit 1; }
	command -v blkid >/dev/null
'
uuid=$(cat /proc/sys/kernel/random/uuid)
mkdir "$tmp/seed"
printf '%s\n' "$uuid" > "$tmp/seed/hotplug-source"
disk=$(mktemp "$jail/hotplug-XXXXXXXX")
truncate -s 32M "$disk"
mkfs.ext4 -q -U "$uuid" -d "$tmp/seed" "$disk"

session=$(osb_api command session create "$VERIFY_ID" -o json | jq -er '.session_id | strings | select(length > 0)')
# Run a single foreground command in a named execd session. The process
# that starts before attach must itself see the seeded disk and write to it.
osb_api command session run "$VERIFY_ID" "$session" --timeout 2m -- sh -ceu '
	n=0
	while :; do
		printf "tick %s %s\n" "$$" "$n"
		if [ -f /data/hotplug-source ] && [ "$(cat /data/hotplug-source)" = "$1" ]; then
			printf "%s\n" "$1" > /data/hotplug-result
			printf "mounted %s %s\n" "$$" "$n"
			exit 0
		fi
		n=$((n + 1))
		sleep 0.25
	done
' sh "$uuid" > "$tmp/stream" &
stream_pid=$!
wait_for "live ticks from session $session" 10 grep -Eq '^tick [0-9]+ 1$' "$tmp/stream"

# Relative socket name stays within Linux's AF_UNIX path length limit.
payload=$(jq -n --arg path "/${disk##*/}" '{drive_id:"probe",path_on_host:$path,is_root_device:false,is_read_only:false}')
(cd "$jail" && curl --silent --show-error --fail-with-body --max-time 10 --unix-socket api.sock \
	-X PUT -H 'Content-Type: application/json' -d "$payload" http://localhost/drives/probe) \
	|| die "live PUT /drives/probe failed; check the restore launcher's --enable-pci flag"

guest sh -ceu '
	echo 1 > /sys/bus/pci/rescan
	for attempt in $(seq 1 40); do
		for dev in /dev/vd*; do
			[ -b "$dev" ] || continue
			[ "$(blkid -p -s UUID -o value "$dev")" = "$1" ] || continue
			mkdir -p /data
			mount -t ext4 -o rw "$dev" /data
			exit 0
		done
		sleep 0.25
	done
	echo "disk UUID $1 missing after PCI rescan" >&2
	exit 1
' sh "$uuid"

wait "$stream_pid"
stream_pid=
awk '
	$1 == "tick" && NF == 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ {
		if (ticks == 0) pid=$2
		if (done || $2 != pid || $3 != ticks) exit 1
		ticks++; next
	}
	$1 == "mounted" && NF == 3 && !done && $2 == pid && $3 == ticks-1 { done=1; next }
	{ exit 1 }
	END { if (!done || ticks < 2) exit 1 }
' "$tmp/stream" || die "PID, sequence or completion mismatch in session $session"

guest umount /data
result=$(debugfs -R 'cat /hotplug-result' "$disk")
[[ "$result" == "$uuid" ]] || die "the original session's write did not reach the test disk"
pass "template $TEMPLATE_ID: session $session survived hotplug ($(tail -1 "$tmp/stream")); disk UUID $uuid verified"
