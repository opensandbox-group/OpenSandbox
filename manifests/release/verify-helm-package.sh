#!/usr/bin/env bash

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

set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  manifests/release/verify-helm-package.sh <package> <component> <chart-version> <app-version>

Verifies the metadata and default rendering of an already packaged Helm chart.
Charts without an appVersion (base, ingress-gateway) pass an empty
<app-version>; for the all-in-one opensandbox chart it also verifies that all
embedded charts are present and that the default controller arguments omit the
optional containerd socket flag.
EOF
}

die() {
  echo "[helm-package][error] $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

metadata_value() {
  local key="$1"
  local metadata="$2"
  awk -F': *' -v key="$key" '$1 == key {gsub(/^"|"$/, "", $2); print $2; exit}' <<<"$metadata"
}

if [[ $# -ne 4 ]]; then
  usage >&2
  exit 2
fi

package_path="$1"
expected_component="$2"
expected_chart_version="$3"
expected_app_version="$4"

require_cmd helm
require_cmd tar
require_cmd awk
require_cmd grep
require_cmd mktemp

[[ -f "$package_path" ]] || die "Package does not exist: $package_path"

expected_name="${expected_component}-${expected_chart_version}.tgz"
actual_name="$(basename "$package_path")"
[[ "$actual_name" == "$expected_name" ]] || \
  die "Expected package name '$expected_name', got '$actual_name'"

metadata="$(helm show chart "$package_path")"
actual_component="$(metadata_value name "$metadata")"
actual_chart_version="$(metadata_value version "$metadata")"
actual_app_version="$(metadata_value appVersion "$metadata")"

[[ "$actual_component" == "$expected_component" ]] || \
  die "Expected chart name '$expected_component', got '$actual_component'"
[[ "$actual_chart_version" == "$expected_chart_version" ]] || \
  die "Expected chart version '$expected_chart_version', got '$actual_chart_version'"
if [[ -n "$expected_app_version" ]]; then
  [[ "$actual_app_version" == "$expected_app_version" ]] || \
    die "Expected app version '$expected_app_version', got '$actual_app_version'"
else
  [[ -z "$actual_app_version" ]] || \
    die "Chart unexpectedly declares appVersion '$actual_app_version' (expected none)"
fi

helm lint "$package_path"

work_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT

tar -tzf "$package_path" >"$work_dir/archive-files.txt"
helm template release-under-test "$package_path" \
  --namespace opensandbox-system \
  >"$work_dir/rendered.yaml" \
  2>"$work_dir/render-warnings.txt"

check_primary_image=1
case "$expected_component" in
  opensandbox|opensandbox-server) expected_primary_image_suffix="/server:release-${expected_app_version}" ;;
  opensandbox-controller) expected_primary_image_suffix="/controller:release-${expected_app_version}" ;;
  opensandbox-node-agent) expected_primary_image_suffix="/nodeagent:release-${expected_app_version}" ;;
  base) check_primary_image= ;;  # cluster-scoped resources only, no workloads
  ingress-gateway) check_primary_image= ;;  # image tag is independent of chart releases
  *) die "Unsupported Helm component: ${expected_component}" ;;
esac
if [[ -n "$check_primary_image" ]]; then
  # CRD schemas may declare properties named "image" (multi-line, no inline
  # value); skip CRD documents so they are not mistaken for container images.
  awk '
    /^kind: CustomResourceDefinition$/ { in_crd = 1 }
    /^---$/ { in_crd = 0 }
    !in_crd && $1 == "image:" { gsub(/^"|"$/, "", $2); print $2 }
  ' "$work_dir/rendered.yaml" \
    >"$work_dir/rendered-images.txt"
  awk -v suffix="$expected_primary_image_suffix" '
    length($0) >= length(suffix) && substr($0, length($0) - length(suffix) + 1) == suffix { found = 1 }
    END { exit !found }
  ' "$work_dir/rendered-images.txt" || \
    die "Default primary image does not match appVersion ${expected_app_version} (${expected_primary_image_suffix})"
fi

case "$expected_component" in
  base)
    # The base chart ships the OpenSandbox CRDs plus the fast-sandbox CRDs
    # (sandbox.fast.io, pinned in manifests/third-party/fast-sandbox.commit).
    for crd in \
      batchsandboxes.sandbox.opensandbox.io \
      pools.sandbox.opensandbox.io \
      sandboxsnapshots.sandbox.opensandbox.io \
      sandboxes.sandbox.fast.io \
      sandboxpools.sandbox.fast.io \
      sandboxsnapshots.sandbox.fast.io \
      sandboxtemplates.sandbox.fast.io; do
      grep -Fq "name: ${crd}$" "$work_dir/rendered.yaml" ||
        die "base package did not render the CRD ${crd}"
    done
    ;;
  ingress-gateway)
    grep -Fq 'name: opensandbox-ingress-gateway' "$work_dir/rendered.yaml" || \
      die "ingress-gateway package did not render the gateway resources"
    ;;
esac

if [[ "$expected_component" == "opensandbox" ]]; then
  for dependency in opensandbox-controller opensandbox-server opensandbox-node-agent base ingress-gateway; do
    grep -Fxq "opensandbox/charts/${dependency}/Chart.yaml" "$work_dir/archive-files.txt" || \
      die "All-in-one package is missing embedded chart: $dependency"
  done

  grep -Fq 'name: opensandbox-controller-manager' "$work_dir/rendered.yaml" || \
    die "All-in-one package did not render the controller Deployment"
  grep -Fq 'name: opensandbox-server' "$work_dir/rendered.yaml" || \
    die "All-in-one package did not render the server resources"

  embedded_server_metadata="$(
    tar -xOzf "$package_path" opensandbox/charts/opensandbox-server/Chart.yaml
  )"
  embedded_server_app_version="$(metadata_value appVersion "$embedded_server_metadata")"
  [[ "$embedded_server_app_version" == "$expected_app_version" ]] || \
    die "Umbrella appVersion ${expected_app_version} does not match embedded server appVersion ${embedded_server_app_version}"

  if grep -Fq -- '--containerd-socket-path' "$work_dir/rendered.yaml"; then
    die "Default rendering unexpectedly contains --containerd-socket-path"
  fi
fi

echo "Verified Helm package: ${actual_name}"
echo "  chart: ${actual_component} ${actual_chart_version}"
echo "  app:   ${actual_app_version}"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    echo "## Helm package verified"
    echo
    echo "- Package: \`${actual_name}\`"
    echo "- Chart version: \`${actual_chart_version}\`"
    echo "- App version: \`${actual_app_version}\`"
    if [[ "$expected_component" == "opensandbox" ]]; then
      echo "- Embedded charts: controller, server, node-agent"
      echo "- Default containerd socket flag: omitted"
    fi
  } >>"$GITHUB_STEP_SUMMARY"
fi
