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

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

command -v uv >/dev/null 2>&1 || {
  echo "[helm-smoke][error] Missing required command: uv" >&2
  exit 1
}

cd "${repo_root}/tests/python"
uv sync --frozen
uv run pytest \
  tests/test_sandbox_e2e_sync.py::TestSandboxE2ESync::test_01_sandbox_lifecycle_and_health \
  tests/test_sandbox_e2e_sync.py::TestSandboxE2ESync::test_02_basic_command_execution
