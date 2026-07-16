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

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
script="$repo_root/sandboxes/code-interpreter/scripts/code-interpreter.sh"
sandbox=$(mktemp -d)
trap 'rm -rf "$sandbox"' EXIT

write_tslab_stub() {
	cat >"$sandbox/tslab" <<'STUB'
#!/usr/bin/env bash
echo "tslab $*" >>"$CALL_LOG"
STUB
	chmod +x "$sandbox/tslab"
}

cat >"$sandbox/npm" <<'STUB'
#!/usr/bin/env bash
echo "npm $*" >>"$CALL_LOG"
if [ "$1 $2 $3" = "install -g tslab" ]; then
	cat >"$TEST_BIN_DIR/tslab" <<'TSLAB'
#!/usr/bin/env bash
echo "tslab $*" >>"$CALL_LOG"
TSLAB
	chmod +x "$TEST_BIN_DIR/tslab"
fi
STUB
write_tslab_stub
cat >"$sandbox/jupyter" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = "kernelspec" ] && [ "$2" = "list" ]; then
	cat "$JUPYTER_KERNELS"
fi
STUB
chmod +x "$sandbox/npm" "$sandbox/jupyter"

extract_node_setup() {
	awk '
		/^tslab_kernels_installed\(\)/ {emit=1}
		emit {print}
		/^setup_node\(\)/ {in_setup=1}
		in_setup && /^}/ {exit}
	' "$script"
}

run_setup_node() {
	CALL_LOG=$1 JUPYTER_KERNELS=$2 TEST_BIN_DIR="$sandbox" PATH="$sandbox:$PATH" bash -c "$(extract_node_setup); setup_node"
}

call_log="$sandbox/calls-installed.log"
kernels="$sandbox/kernels-installed.txt"
cat >"$kernels" <<'KERNELS'
Available kernels:
  python    /usr/local/share/jupyter/kernels/python
  tslab     /usr/local/share/jupyter/kernels/tslab
  jslab     /usr/local/share/jupyter/kernels/jslab
KERNELS
run_setup_node "$call_log" "$kernels"
if [ -s "$call_log" ]; then
	echo "expected no npm or tslab calls when kernels are already installed" >&2
	cat "$call_log" >&2
	exit 1
fi

call_log="$sandbox/calls-missing-kernels.log"
kernels="$sandbox/kernels-missing.txt"
cat >"$kernels" <<'KERNELS'
Available kernels:
  python    /usr/local/share/jupyter/kernels/python
KERNELS
run_setup_node "$call_log" "$kernels"
if grep -q '^npm install -g tslab$' "$call_log"; then
	echo "did not expect npm install when tslab command exists" >&2
	cat "$call_log" >&2
	exit 1
fi
if ! grep -q '^tslab install$' "$call_log"; then
	echo "expected tslab install when kernels are missing" >&2
	cat "$call_log" >&2
	exit 1
fi

call_log="$sandbox/calls-partial-kernels.log"
kernels="$sandbox/kernels-partial.txt"
cat >"$kernels" <<'KERNELS'
Available kernels:
  python    /usr/local/share/jupyter/kernels/python
  tslab     /usr/local/share/jupyter/kernels/tslab
KERNELS
run_setup_node "$call_log" "$kernels"
if grep -q '^npm install -g tslab$' "$call_log"; then
	echo "did not expect npm install when tslab command exists" >&2
	cat "$call_log" >&2
	exit 1
fi
if ! grep -q '^tslab install$' "$call_log"; then
	echo "expected tslab install when one kernel is missing" >&2
	cat "$call_log" >&2
	exit 1
fi

rm "$sandbox/tslab"
call_log="$sandbox/calls-missing-command.log"
run_setup_node "$call_log" "$kernels"
if ! grep -q '^npm install -g tslab$' "$call_log"; then
	echo "expected npm install when tslab command is missing" >&2
	cat "$call_log" >&2
	exit 1
fi
