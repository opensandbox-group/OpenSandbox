#!/bin/sh
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
#
# Pre-start hook for opensandbox-supervisor wrapping the egress worker.
# Reaps any mitmdump left over from a previous crashed egress so the next
# launch can bind the transparent-MITM listen port (default 18081).
#
# Scope is deliberately narrow:
#   * iptables NAT rules are NOT torn down here. The egress sidecar shares
#     a network namespace with the workload it protects; tearing rules
#     down between crashes would leave the workload with unfiltered egress
#     for the full backoff window. Egress's own SetupRedirect is additive
#     and tolerates pre-existing rules (first match wins).
#   * The `inet opensandbox` nft table is NOT touched here either. The
#     egress nftables manager already prepends `delete table inet
#     opensandbox` to its ruleset script, so ApplyStatic is idempotent.
#
# Hard contract: this script MUST NOT exit non-zero. A misbehaving cleanup
# hook is worse than a stray mitmdump; supervisor would treat the hook
# failure as a launch attempt and trip its crashloop budget faster.

# Intentionally no `set -e`. `set -u` for typo safety on env names only.
set -u

log() { printf '[egress-cleanup] %s\n' "$*" >&2; }

# Wraps a command so non-zero exit is silently absorbed. Output goes to
# stderr so it shows up in container logs without polluting the event log.
try() { "$@" 2>&1 | sed 's/^/  /' >&2; return 0; }

# ─── stray mitmdump (orphaned after hard crash) ──────────────────────
kill_stray_mitmdump() {
  command -v pkill >/dev/null 2>&1 || { log "pkill not present; skipping mitmdump reap"; return 0; }
  # mitmdump runs as the `mitmproxy` user (uid 10042 per egress Dockerfile).
  # `-u mitmproxy` scopes pkill to that uid so we never touch anything else;
  # `-f mitmdump` is the cmdline match safety net inside that uid.
  # SIGTERM first; give it a moment; SIGKILL anything that ignored TERM.
  try pkill -TERM -u mitmproxy -f mitmdump
  # Short sleep, but bounded so this hook still finishes inside the
  # supervisor's PreStartTimeout (default 30s) with plenty of headroom.
  sleep 1
  try pkill -KILL -u mitmproxy -f mitmdump
  log "stray mitmdump processes reaped (best-effort)"
}

main() {
  log "starting (worker_exit_code=${WORKER_EXIT_CODE:-?} signal=${WORKER_SIGNAL:-?} attempt=${WORKER_ATTEMPT:-?})"
  kill_stray_mitmdump
  log "done"
  exit 0
}

# Trap unexpected interpreter errors so we still exit 0.
trap 'log "cleanup hit shell error on line $LINENO; exiting 0 anyway"; exit 0' HUP INT TERM
main "$@" || true
exit 0
