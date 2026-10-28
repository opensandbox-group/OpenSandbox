#!/bin/bash
# Copyright 2025 The OpenSandbox Authors
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

# Java SDK smoke test for the execd MCP proxy bridge. Drives a RUNNING execd
# (see tests/mcp_smoke.sh for the harness) with the official java SDK.
#
# Env: BASE_URL, API_TOKEN. Requires JDK 17+ and Maven; skips otherwise.

set -euo pipefail

cd "$(dirname "$0")/java-smoke"

if ! command -v mvn >/dev/null 2>&1; then
	echo "SKIP: maven not available for java MCP smoke"
	exit 0
fi

if command -v /usr/libexec/java_home >/dev/null 2>&1; then
	JAVA17_HOME="$(/usr/libexec/java_home -v 17+ 2>/dev/null || true)"
	if [[ -z "${JAVA17_HOME}" ]]; then
		echo "SKIP: no JDK 17+ available for java MCP smoke"
		exit 0
	fi
	export JAVA_HOME="${JAVA17_HOME}"
elif [[ ! "${JAVA_HOME:-}" ]]; then
	echo "SKIP: no JDK 17+ available for java MCP smoke"
	exit 0
fi

mvn -q -B compile exec:java
