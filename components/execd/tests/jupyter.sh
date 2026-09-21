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


export JUPYTER_PORT=54321
export JUPYTER_TOKEN=opensandboxexecdintegrationtest

install_jupyter() {
	# install jupyter notebook for integration testing
	python --version
	pip install ipykernel jupyter

	echo "Starting jupyter notebook ..."
	jupyter notebook --ip=0.0.0.0 --port=$JUPYTER_PORT --allow-root --no-browser --NotebookApp.token=$JUPYTER_TOKEN >/tmp/jupyter.log 2>&1 &

	sleep 3
}
