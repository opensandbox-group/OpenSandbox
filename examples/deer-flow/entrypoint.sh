#!/usr/bin/env bash
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

# Container entrypoint: point DeerFlow at OpenSandbox, start the Web UI
# stack in the background, then run the OpenSandbox server in the foreground.
set -euo pipefail

echo "=============================================================="
echo " OpenSandbox x DeerFlow all-in-one image"
echo "   DeerFlow Web UI : http://localhost:2026   (nginx entry point)"
echo "   OpenSandbox API : http://localhost:8080   (runtime=docker)"
echo "   DeerFlow source : /opt/deer-flow          (backend venv on PATH)"
echo "   OpenSandbox checkout: /opt/OpenSandbox"
echo "   Commands:"
echo "     enable-opensandbox   switch config.yaml back to OpenSandboxProvider"
echo "     make stop            stop the Web UI stack (gateway/frontend/nginx)"
echo "     deerflow --print \"...\"  one headless agent turn (model required)"
echo "     make setup           (in /opt/deer-flow) configure models via wizard"
echo "=============================================================="

# Ensure config.yaml exists AND points at OpenSandboxProvider on every boot:
# the Web first-run setup regenerates config.yaml with the default
# LocalSandboxProvider, which would silently route agent tools back to the
# gateway host. enable-opensandbox is idempotent and only touches the
# sandbox.use line, leaving model keys and other user settings alone.
enable-opensandbox

# Nginx is the only externally bound entry (gateway/frontend stay on
# loopback), so it must listen on all interfaces for port mapping to work.
export BIND_HOST=0.0.0.0

echo "[image] Starting DeerFlow Web UI stack (prod mode, background)..."
# --skip-install: dependencies are baked at build time (uv sync would drop
#   the opensandbox extra if UV_EXTRAS were unset; it is set in the image).
# --skip-frontend-build: frontend/.next is pre-built; reuse via next start.
bash scripts/serve.sh --prod --daemon --skip-install --skip-frontend-build

echo "[image] Starting OpenSandbox server on :8080 (sandboxes run on the host Docker)"
exec opensandbox-server
