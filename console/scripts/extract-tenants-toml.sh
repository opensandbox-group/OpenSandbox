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

# 从 tenants ConfigMap YAML 抽出纯 tenants.toml，供 BFF 本地 TENANTS_TOML_PATH 使用。
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONSOLE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
CM="${1:-$CONSOLE_DIR/k8s/tenants-configmap.example.yaml}"
OUT="${2:-$CONSOLE_DIR/bff/tenants.local.toml}"
python3 - <<PY
import sys, pathlib, re
text = pathlib.Path("$CM").read_text()
match = re.search(r"tenants\\.toml:\\s*\\|\\s*\\n((?:    .+\\n)+)", text)
if not match:
    sys.exit("Could not find data.tenants.toml block in ConfigMap")
block = match.group(1)
lines = [ln[4:] if ln.startswith("    ") else ln for ln in block.splitlines()]
pathlib.Path("$OUT").write_text("\\n".join(lines).rstrip() + "\\n")
print("Wrote", "$OUT")
PY
