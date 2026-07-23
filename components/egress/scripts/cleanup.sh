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
# launch can bind the transparent-MITM listen port (default 18081), and
# tears down stale iptables DNS redirect rules so DNS queries reach the
# real nameserver while the new egress process is still initializing.
#
# Scope:
#   * iptables NAT OUTPUT rules for port 53 ARE removed here. Without
#     this, a crashed egress leaves REDIRECT rules pointing at a dead
#     proxy (127.0.0.1:15353), causing DNS resolution failure for the
#     entire restart window. The new egress process re-installs them
#     after its DNS proxy is listening, so the unfiltered window is
#     limited to the startup duration (~200ms).
#   * The `inet/ip/ip6 opensandbox_dns_redirect` nft tables ARE removed
#     here. Native nft DNS fallback uses those tables when iptables-nft
#     cannot append OUTPUT rules; after a crash they would otherwise keep
#     redirecting DNS to a dead proxy.
#   * The `inet opensandbox` nft table is NOT touched here. The egress
#     nftables manager already prepends `delete table inet opensandbox` to
#     its ruleset script, so ApplyStatic is idempotent.
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

# ─── stale iptables rules (DNS redirect + mitmproxy transparent) ────
# When egress crashes, iptables REDIRECT rules survive in the shared
# network namespace. DNS rules point port 53 at a dead proxy (15353);
# mitmproxy rules point 80/443 at a dead mitmdump. Remove both so
# traffic flows normally until the new egress re-installs them.
remove_stale_iptables() {
  for cmd in iptables ip6tables; do
    command -v "$cmd" >/dev/null 2>&1 || continue
    # Delete all nat/OUTPUT rules that redirect port 53 (DNS proxy).
    "$cmd" -t nat -S OUTPUT 2>/dev/null \
      | awk '/--dport 53/ {sub(/^-A/,"-D"); print}' \
      | while IFS= read -r rule; do
          eval "$cmd -t nat $rule" 2>/dev/null || true
        done
    # Delete all nat/OUTPUT rules that redirect ports 80,443 (mitmproxy transparent).
    "$cmd" -t nat -S OUTPUT 2>/dev/null \
      | awk '/--dports 80,443/ {sub(/^-A/,"-D"); print}' \
      | while IFS= read -r rule; do
          eval "$cmd -t nat $rule" 2>/dev/null || true
        done
  done
  log "stale iptables rules removed (best-effort)"
}

remove_stale_dns_redirect_nft() {
  command -v nft >/dev/null 2>&1 || { log "nft not present; skipping native DNS redirect cleanup"; return 0; }
  for family in inet ip ip6; do
    printf 'delete table %s opensandbox_dns_redirect\n' "$family" | nft -f - 2>/dev/null || true
  done
  log "stale native DNS redirect nft tables removed (best-effort)"
}

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
  remove_stale_dns_redirect_nft
  remove_stale_iptables
  kill_stray_mitmdump
  log "done"
  exit 0
}

# Trap unexpected interpreter errors so we still exit 0.
trap 'log "cleanup hit shell error on line $LINENO; exiting 0 anyway"; exit 0' HUP INT TERM
main "$@" || true
exit 0
