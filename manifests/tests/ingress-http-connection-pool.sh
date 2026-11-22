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
values_fixture="$(mktemp)"
trap 'rm -f "$values_fixture"' EXIT

# 省略默认参数依赖 Go 与 chart 的默认值一致；注册值变更时必须一起更新兼容规则。
for registration in \
  'flag.IntVar(&HTTPMaxIdleConns, "http-max-idle-conns", 100,' \
  'flag.IntVar(&HTTPMaxIdleConnsPerHost, "http-max-idle-conns-per-host", 2,' \
  'flag.DurationVar(&HTTPIdleConnTimeout, "http-idle-conn-timeout", 90*time.Second,'; do
  if ! grep -Fq -- "$registration" "$repo_root/components/ingress/pkg/flag/parser.go"; then
    echo "Go HTTP connection pool default no longer matches chart compatibility defaults: $registration" >&2
    exit 1
  fi
done

assert_pool_args() {
  local rendered="$1"
  local max_idle="$2"
  local per_host="$3"
  local idle_timeout="$4"
  local argument
  if grep -Fq -- '<no value>' <<<"$rendered"; then
    echo "Rendered ingress arguments contain <no value>" >&2
    exit 1
  fi
  if grep -Eq -- '--http-max-idle-conns(-per-host)?=[^"]*[eE][+-]?[0-9]+' <<<"$rendered"; then
    echo "Rendered integer arguments contain scientific notation" >&2
    exit 1
  fi
  for argument in \
    "--http-max-idle-conns=$max_idle" \
    "--http-max-idle-conns-per-host=$per_host" \
    "--http-idle-conn-timeout=$idle_timeout"; do
    # 空期望值表示该参数应省略，保障旧镜像兼容。
    if [[ "$argument" == *= ]]; then
      if grep -Fq -- "\"$argument" <<<"$rendered"; then
        echo "Unexpected ingress argument: $argument" >&2
        exit 1
      fi
    elif ! grep -Fq -- "\"$argument\"" <<<"$rendered"; then
      echo "Missing ingress argument: $argument" >&2
      exit 1
    fi
  done
}

helm dependency build "$umbrella_chart"

for chart in standalone umbrella; do
  chart_path="$standalone_chart"
  prefix="gateway.httpConnectionPool"
  values_indent=""
  render_args=(--show-only templates/deployment.yaml)
  if [[ "$chart" == umbrella ]]; then
    chart_path="$umbrella_chart"
    prefix="ingress-gateway.gateway.httpConnectionPool"
    values_indent="  "
    render_args=(--show-only charts/ingress-gateway/templates/deployment.yaml --set ingress-gateway.enabled=true)
  fi

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}")"
  assert_pool_args "$rendered" "" "" ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConns=2048")"
  assert_pool_args "$rendered" 2048 "" ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConnsPerHost=4")"
  assert_pool_args "$rendered" "" 4 ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.idleConnTimeout=2m")"
  assert_pool_args "$rendered" "" "" 2m

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
  assert_pool_args "$rendered" "" "" ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConns=null" --set "$prefix.maxIdleConnsPerHost=4")"
  assert_pool_args "$rendered" "" 4 ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.maxIdleConnsPerHost=null" --set "$prefix.maxIdleConns=2048")"
  assert_pool_args "$rendered" 2048 "" ""

  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" \
    --set "$prefix.idleConnTimeout=null" --set "$prefix.maxIdleConns=2048")"
  assert_pool_args "$rendered" 2048 "" ""

  # values 文件中的数值走 YAML 解码路径，不能只用 --set 的整数类型替代回归。
  if [[ "$chart" == umbrella ]]; then
    printf 'ingress-gateway:\n' >"$values_fixture"
  else
    : >"$values_fixture"
  fi
  cat >>"$values_fixture" <<YAML
${values_indent}gateway:
${values_indent}  httpConnectionPool:
${values_indent}    maxIdleConns: 1000000
${values_indent}    maxIdleConnsPerHost: 1000000
${values_indent}    idleConnTimeout: null
YAML
  rendered="$(helm template pool-test "$chart_path" "${render_args[@]}" -f "$values_fixture")"
  assert_pool_args "$rendered" 1000000 1000000 ""
  echo "Verified $chart ingress HTTP connection pool arguments"
done
