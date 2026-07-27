#!/bin/bash
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

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

CA_PATH="$TEST_DIR/mitmproxy-ca-cert.pem"
MERGED_PATH="$TEST_DIR/merged-ca-certificates.pem"
EXECD_MARKER="$TEST_DIR/execd-started"
TEST_BOOTSTRAP="$TEST_DIR/bootstrap.sh"
FAILED_BOOTSTRAP="$TEST_DIR/bootstrap-failed-refresh.sh"
MOCK_BIN="$TEST_DIR/bin"

cp "$ROOT_DIR/bootstrap.sh" "$TEST_BOOTSTRAP"
sed -i \
	-e "s|^MITM_CA=.*|MITM_CA=\"$CA_PATH\"|" \
	-e "s|^[[:space:]]*merged=\"/opt/opensandbox/merged-ca-certificates.pem\"|\tmerged=\"$MERGED_PATH\"|" \
	-e "s|for search_dir in /usr/lib/jvm /usr/java /opt/java|for search_dir in \"$TEST_DIR/no-jdks\"|" \
	-e "s|for d in /opt/jdk\*|for d in \"$TEST_DIR/no-jdks\"/*|" \
	"$TEST_BOOTSTRAP"

mkdir -p "$MOCK_BIN"
for command_name in cat cp dirname id mkdir mv rm sh sleep touch tr; do
	ln -s "$(command -v "$command_name")" "$MOCK_BIN/$command_name"
done

# Keep the bootstrap JDK scan isolated from the host toolchain.
unset JAVA_HOME

cat > "$TEST_DIR/execd" <<EOF
#!/bin/sh
touch "$EXECD_MARKER"
EOF
chmod +x "$TEST_DIR/execd" "$TEST_BOOTSTRAP"

cp /etc/ssl/certs/ca-certificates.crt "$CA_PATH"

OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true \
	EXECD="$TEST_DIR/execd" \
	PATH="$MOCK_BIN" \
	sh "$TEST_BOOTSTRAP" --refresh-mitm-ca-trust

test -s "$MERGED_PATH"
if compgen -G "${MERGED_PATH}.tmp.*" >/dev/null; then
	echo "FAIL: refresh left a temporary merged CA bundle" >&2
	exit 1
fi
if test -e "$EXECD_MARKER"; then
	echo "FAIL: refresh-only mode started execd" >&2
	exit 1
fi

cp "$TEST_BOOTSTRAP" "$FAILED_BOOTSTRAP"
sed -i "s|merged=\"$MERGED_PATH\"|merged=\"$TEST_DIR/missing/merged-ca-certificates.pem\"|" "$FAILED_BOOTSTRAP"
set +e
OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true \
	EXECD="$TEST_DIR/execd" \
	PATH="$MOCK_BIN" \
	sh "$FAILED_BOOTSTRAP" --refresh-mitm-ca-trust >/dev/null 2>&1
failed_status=$?
set -e
if test "$failed_status" -eq 0; then
	echo "FAIL: strict refresh reported success when the merged bundle could not be written" >&2
	exit 1
fi

OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true \
	BOOTSTRAP_CMD=true \
	EXECD="$TEST_DIR/execd" \
	EXECD_ENVS="$TEST_DIR/.env" \
	PATH="$MOCK_BIN" \
	sh "$FAILED_BOOTSTRAP" >/dev/null 2>&1
for _ in $(seq 1 20); do
	test -e "$EXECD_MARKER" && break
	sleep 0.01
done
test -e "$EXECD_MARKER"

echo "PASS: strict refresh reports failures while initial bootstrap remains best-effort"
