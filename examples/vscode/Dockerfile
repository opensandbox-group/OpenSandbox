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

FROM debian:12-slim

#----------------------
# Install all prerequisite packages in one layer
RUN apt-get update && apt-get install -y \
    python3 \
    python3-pip \
    curl \
    ca-certificates \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

#----------------------
# Install code-server
RUN curl -fsSL https://code-server.dev/install.sh | sh

#----------------------
# Create a non-root user
RUN groupadd -r vscode && useradd -r -g vscode vscode \
    && mkdir -p /home/vscode /workspace && chown -R vscode:vscode /home/vscode /workspace

#----------------------
# Configure user, etc

WORKDIR /workspace

USER vscode

# Default to bash
CMD ["bash"]
