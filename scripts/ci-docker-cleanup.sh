#!/usr/bin/env bash
set -euo pipefail

echo "Disk usage before Docker cleanup:"
df -h /

containers="$(timeout 30s docker ps -aq || true)"
if [ -n "${containers}" ]; then
  echo "${containers}" | xargs -r docker rm -f || true
fi

timeout 60s docker builder prune -af || true
timeout 120s docker system prune -af --volumes || true
timeout 30s rm -rf "${HOME:-/home/admin}/.docker/buildx/activity"/* || true

echo "Disk usage after Docker cleanup:"
df -h /
