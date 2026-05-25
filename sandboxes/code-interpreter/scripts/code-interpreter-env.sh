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

# This script is used to switch versions of different languages in the OpenSandbox environment
# Usage: source /opt/opensandbox/code-interpreter-env.sh <language> <version>
# Examples:
#   source /opt/opensandbox/code-interpreter-env.sh python 3.13
#   source /opt/opensandbox/code-interpreter-env.sh java 21
#   source /opt/opensandbox/code-interpreter-env.sh node 22
#   source /opt/opensandbox/code-interpreter-env.sh go 1.25

function usage() {
	echo "Usage: source code-interpreter-env.sh <language> <version>"
	echo "Supported languages: python, java, node, go"
}

DEFAULT_PY_VERSION=${DEFAULT_PY_VERSION:-3.13}
DEFAULT_JAVA_VERSION=${DEFAULT_JAVA_VERSION:-21}
DEFAULT_NODE_VERSION=${DEFAULT_NODE_VERSION:-22}
DEFAULT_GO_VERSION=${DEFAULT_GO_VERSION:-1.25}

append_env_if_needed() {
	local key=$1
	local value=$2
	if [ -z "${EXECD_ENVS:-}" ]; then
		return
	fi
	# Best-effort: ensure parent dir exists, ignore errors.
	mkdir -p "$(dirname "$EXECD_ENVS")" 2>/dev/null || true
	printf '%s=%s\n' "$key" "$value" >>"$EXECD_ENVS" 2>/dev/null || true
}

function switch_python() {
	local version=$1
	if [ -z "$version" ]; then
		echo "Available Python versions:"
		find /opt/python/versions -maxdepth 1 -name "cpython-*" -type d -printf "%f\n" | cut -d'-' -f2 | sort -V
		return
	fi

	# Find matching version directory
	local target_dir=$(find /opt/python/versions -maxdepth 1 -type d -name "cpython-${version}*" | sort -V | tail -n 1)

	if [ -d "$target_dir" ]; then
		export PATH="$target_dir/bin:$PATH"
		append_env_if_needed PATH "$PATH"
		echo "Switched to Python $(python3 --version)"
	else
		echo "Python version $version not found."
	fi
}

function switch_java() {
	local version=$1
	if [ -z "$version" ]; then
		echo "Available Java versions:"
		ls /usr/lib/jvm/ | grep -E '^java-[0-9]+-openjdk' | cut -d'-' -f2 | sort -V | uniq
		return
	fi

	# Match openjdk path
	local java_home=""
	if [ -d "/usr/lib/jvm/java-${version}-openjdk-amd64" ]; then
		java_home="/usr/lib/jvm/java-${version}-openjdk-amd64"
	elif [ -d "/usr/lib/jvm/java-${version}-openjdk-arm64" ]; then # ARM compatibility
		java_home="/usr/lib/jvm/java-${version}-openjdk-arm64"
	fi

	if [ -n "$java_home" ]; then
		export JAVA_HOME="$java_home"
		export PATH="$JAVA_HOME/bin:$PATH"
		append_env_if_needed JAVA_HOME "$JAVA_HOME"
		append_env_if_needed PATH "$PATH"
		echo "Switched to Java $version ($JAVA_HOME)"
	else
		echo "Java version $version not found."
	fi
}

function switch_node() {
	local version=$1
	if [ -z "$version" ]; then
		echo "Available Node versions:"
		ls /opt/node/
		return
	fi

	# Find matching version (e.g. v18 -> v18.x.x)
	local target_dir=$(find /opt/node -maxdepth 1 -type d -name "v${version}*" | sort -V | tail -n 1)

	if [ -d "$target_dir" ]; then
		export PATH="$target_dir/bin:$PATH"
		append_env_if_needed PATH "$PATH"
		echo "Switched to Node $(node --version)"
	else
		echo "Node version $version not found."
	fi
}

function switch_go() {
	local version=$1
	if [ -z "$version" ]; then
		echo "Available Go versions:"
		ls /opt/go/
		return
	fi

	# Find matching version
	local target_dir=$(find /opt/go -maxdepth 1 -type d -name "${version}*" | sort -V | tail -n 1)

	if [ -d "$target_dir" ]; then
		export GOROOT="$target_dir"
		export PATH="$GOROOT/bin:$PATH"
		append_env_if_needed GOROOT "$GOROOT"
		append_env_if_needed PATH "$PATH"
		echo "Switched to Go $(go version)"
	else
		echo "Go version $version not found."
	fi
}

# Main logic
LANG=$1
VER=$2

if [ -z "$LANG" ]; then
	usage
	return
fi

case $LANG in
python)
	if [ -z "$VER" ]; then
		VER=$DEFAULT_PY_VERSION
	fi
	switch_python $VER
	;;
java)
	if [ -z "$VER" ]; then
		VER=$DEFAULT_JAVA_VERSION
	fi
	switch_java $VER
	;;
node)
	if [ -z "$VER" ]; then
		VER=$DEFAULT_NODE_VERSION
	fi
	switch_node $VER
	;;
go)
	if [ -z "$VER" ]; then
		VER=$DEFAULT_GO_VERSION
	fi
	switch_go $VER
	;;
*)
	echo "Unsupported language: $LANG"
	usage
	;;
esac
