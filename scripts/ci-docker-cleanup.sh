#!/usr/bin/env bash
set -euo pipefail

echo "Disk usage before Docker cleanup:"
df -h /

containers="$(
  {
    timeout 30s docker ps -aq --filter "label=opensandbox" || true
    timeout 30s docker ps -aq --filter "label=opensandbox.e2e=credential-vault" || true
    timeout 30s docker ps -aq --filter "name=^/opensandbox-e2e-redis$" || true
    timeout 30s docker ps -aq --filter "name=^/opensandbox-e2e-credential-vault-target$" || true
    timeout 30s docker ps -aq --filter "name=^/egress-smoke-" || true
  } | sort -u
)"
if [ -n "${containers}" ]; then
  echo "${containers}" | xargs -r docker rm -f || true
fi

timeout 30s rm -rf "${HOME:-/home/admin}/.docker/buildx/activity"/* || true

echo "Disk usage after Docker cleanup:"
df -h /
