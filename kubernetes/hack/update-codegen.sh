#!/usr/bin/env bash
# Copyright 2025 Alibaba Group Holding Ltd.
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

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..

GOBIN="$(go env GOBIN)"
export GOBIN="${GOBIN:-$(go env GOPATH | cut -d: -f1)/bin}"

CODEGEN_PKG="${CODEGEN_PKG:-$(go env GOMODCACHE)/k8s.io/code-generator@v0.33.0}"

if [ ! -d "${CODEGEN_PKG}" ]; then
    echo "code-generator not found at ${CODEGEN_PKG}"
    echo "Installing k8s.io/code-generator@v0.33.0..."
    go install k8s.io/code-generator/cmd/client-gen@v0.33.0
    go install k8s.io/code-generator/cmd/lister-gen@v0.33.0
    go install k8s.io/code-generator/cmd/informer-gen@v0.33.0
fi

source "${CODEGEN_PKG}/kube_codegen.sh"

kube::codegen::gen_client \
    --with-watch \
    --output-dir "${SCRIPT_ROOT}/pkg/client" \
    --output-pkg "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client" \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    "${SCRIPT_ROOT}/apis"

echo "Code generation completed successfully!"
