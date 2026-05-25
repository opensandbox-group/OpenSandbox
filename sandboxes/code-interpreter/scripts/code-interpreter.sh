#!/bin/bash
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

# If EXECD_CLONE3_COMPAT is set (same accepted values as execd's pkg/clone3compat), re-exec this
# script under AkihiroSuda/clone3-workaround so the rest of startup runs with clone3 -> ENOSYS.
# See https://github.com/AkihiroSuda/clone3-workaround — binary is /usr/local/bin/clone3-workaround (amd64 image only).
_execd_clone3_compat_normalized=$(
	echo "${EXECD_CLONE3_COMPAT:-}" | tr '[:upper:]' '[:lower:]' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//'
)
case "${_execd_clone3_compat_normalized}" in
"" | 0 | false | off | no) ;;
1 | true | yes | on | reexec)
	if [ -z "${_CODE_INTERPRETER_CLONE3_WRAPPED:-}" ]; then
		_ci_script=${BASH_SOURCE[0]}
		if command -v readlink >/dev/null 2>&1 && _rp=$(readlink -f "${_ci_script}" 2>/dev/null); then
			_ci_script=${_rp}
		fi
		if [ -x /usr/local/bin/clone3-workaround ]; then
			export _CODE_INTERPRETER_CLONE3_WRAPPED=1
			exec /usr/local/bin/clone3-workaround /bin/bash "${_ci_script}" "$@"
		else
			echo "code-interpreter: EXECD_CLONE3_COMPAT is set but /usr/local/bin/clone3-workaround is missing (not installed for this architecture); continuing without wrapper" >&2
		fi
	fi
	unset _ci_script _rp
	;;
*)
	echo "code-interpreter: invalid EXECD_CLONE3_COMPAT=${EXECD_CLONE3_COMPAT:-} (use 1, true, yes, on, or reexec)" >&2
	exit 1
	;;
esac
unset _execd_clone3_compat_normalized

if [ -n "${_CODE_INTERPRETER_CLONE3_WRAPPED:-}" ]; then
	unset EXECD_CLONE3_COMPAT
fi

declare -a pids=()
BASHRC_FILE=${BASHRC_FILE:-/root/.bashrc}

record_env_selection() {
	local lang=$1
	local version=$2

	if [ -z "$version" ]; then
		return
	fi

	local tmp_file
	tmp_file=$(mktemp)

	if [ -f "$BASHRC_FILE" ]; then
		grep -vE "^source /opt/opensandbox/code-interpreter-env.sh ${lang}(\\s|$)" "$BASHRC_FILE" >"$tmp_file" || true
	else
		: >"$tmp_file"
	fi

	echo "source /opt/opensandbox/code-interpreter-env.sh ${lang} ${version}" >>"$tmp_file"
	mv "$tmp_file" "$BASHRC_FILE"
}

if [ -n "${PYTHON_VERSION:-}" ]; then
	source /opt/opensandbox/code-interpreter-env.sh python "${PYTHON_VERSION}"
	record_env_selection python "${PYTHON_VERSION}"
else
	source /opt/opensandbox/code-interpreter-env.sh python
fi

if [ -n "${JAVA_VERSION:-}" ]; then
	source /opt/opensandbox/code-interpreter-env.sh java "${JAVA_VERSION}"
	record_env_selection java "${JAVA_VERSION}"
else
	source /opt/opensandbox/code-interpreter-env.sh java
fi

if [ -n "${NODE_VERSION:-}" ]; then
	source /opt/opensandbox/code-interpreter-env.sh node "${NODE_VERSION}"
	record_env_selection node "${NODE_VERSION}"
else
	source /opt/opensandbox/code-interpreter-env.sh node
fi

if [ -n "${GO_VERSION:-}" ]; then
	source /opt/opensandbox/code-interpreter-env.sh go "${GO_VERSION}"
	record_env_selection go "${GO_VERSION}"
else
	source /opt/opensandbox/code-interpreter-env.sh go
fi

setup_python() {
	python --version
	time {
		python3 -m ipykernel install --name python --display-name "Python"
	}
}

setup_java() {
	time {
		python3 /opt/ijava/install.py --sys-prefix
	}
}

# setup node
setup_node() {
	time {
		npm install -g tslab
		tslab install
	}
}

setup_go() {
	time {
		# shellcheck disable=SC2155
		gonb --install
	}
}

setup_bash() {
	time {
		python3 -m bash_kernel.install
	}
}

# export go bin path
export PATH="$(go env GOPATH)/bin:$PATH"
if [ -n "${EXECD_ENVS:-}" ]; then
	mkdir -p "$(dirname "$EXECD_ENVS")" 2>/dev/null || true
	printf 'PATH=%s\n' "$PATH" >>"$EXECD_ENVS" 2>/dev/null || true
fi

setup_python &
pids+=($!)
setup_java &
pids+=($!)
setup_node &
pids+=($!)
setup_go &
pids+=($!)
setup_bash &
pids+=($!)

jupyter notebook --ip=127.0.0.1 --port="${JUPYTER_PORT:-44771}" --allow-root --no-browser --NotebookApp.token="${JUPYTER_TOKEN:-opensandboxcodeinterpreterjupyter}" >/opt/opensandbox/jupyter.log
