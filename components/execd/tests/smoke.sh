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


set -euxo pipefail

source tests/jupyter.sh
install_jupyter

export EXECD_API_GRACE_SHUTDOWN=500ms
export EXECD_LOG_FILE=execd.log
./bin/execd -jupyter-host=http://127.0.0.1:${JUPYTER_PORT} --jupyter-token=${JUPYTER_TOKEN} --log-level=7 >startup.log 2>&1 &
