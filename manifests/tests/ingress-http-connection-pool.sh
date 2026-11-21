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

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
standalone_chart="$repo_root/manifests/charts/ingress-gateway"
umbrella_chart="$repo_root/manifests/charts/opensandbox"

assert_pool_args() {
  local rendered="$1"
  local max_idle="$2"
  local per_host="$3"
  local idle_timeout="$4"
  for argument in \
    "--http-max-idle-conns=$max_idle" \
    "--http-max-idle-conns-per-host=$per_host" \
    "--http-idle-conn-timeout=$idle_timeout"; do
    if ! grep -Fq -- "\"$argument\"" <<<"$rendered"; then
      echo "Missing ingress argument: $argument" >&2
      exit 1
    fi
  done
}

helm dependency build "$umbrella_chart"

for chart in standalone umbrella; do
  chart_path="$standalone_chart"
  prefix="gateway.httpConnectionPool"
  render_args=(--show-only templates/deployment.yaml)
  if [[ "$chart" == umbrella ]]; then
    chart_path="$umbrella_chart"
    prefix="ingress-gateway.gateway.httpConnectionPool"
    render_args=(--show-only charts/ingress-gateway/templates/deployment.yaml --set ingress-gateway.enabled=true)
  fi

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}")"
  assert_pool_args "$rendered" 100 2 90s

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConns=2048" \
    --set "$prefix.maxIdleConnsPerHost=4" \
    --set "$prefix.idleConnTimeout=2m")"
  assert_pool_args "$rendered" 2048 4 2m

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConns=0" \
    --set "$prefix.maxIdleConnsPerHost=0" \
    --set "$prefix.idleConnTimeout=0s")"
  assert_pool_args "$rendered" 0 0 0s

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" --set "$prefix=null")"
  assert_pool_args "$rendered" 100 2 90s
  echo "Verified $chart ingress HTTP connection pool arguments"
done
