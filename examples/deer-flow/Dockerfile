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
#
# All-in-one lab image: OpenSandbox server + DeerFlow (backend + Web UI).
#
#   - OpenSandbox server (pip package, runtime.type = "docker")
#   - DeerFlow backend (installed from source with the `opensandbox` extra,
#     which ships OpenSandboxProvider)
#   - DeerFlow frontend: pre-built (pnpm install + next build), served by the
#     bundled nginx on :2026 — the single entry point for the Web UI
#   - Python 3.12 + uv, Node.js 22 + corepack/pnpm, Docker CLI (client only),
#     common debugging tools
#   - The OpenSandbox checkout itself (model-free example script + docs)
#
# Sandbox containers run on the HOST Docker daemon: mount docker.sock when
# running this image and the server drives the host daemon through it
# (same topology as server/docker-compose.example.yaml).

# ---- Overridable for network-restricted environments ----
ARG DEERFLOW_REPO=https://github.com/bytedance/deer-flow.git
ARG DEERFLOW_REF=main
ARG OPENSANDBOX_REPO=https://github.com/opensandbox-group/OpenSandbox.git

FROM python:3.12-slim

ARG DEERFLOW_REPO
ARG DEERFLOW_REF
ARG OPENSANDBOX_REPO

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# ---- System and debugging tools (nginx + make serve the DeerFlow Web UI) ----
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git vim nano less htop procps psmisc lsof strace \
      iputils-ping net-tools dnsutils jq openssh-client build-essential tzdata \
      make nginx \
    && rm -rf /var/lib/apt/lists/*

# ---- Docker CLI: inspect sandbox containers on the host (no daemon) ----
COPY --from=docker:cli /usr/local/bin/docker /usr/local/bin/docker

# ---- uv + Node.js (corepack resolves pnpm from DeerFlow's packageManager) ----
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
COPY --from=node:22-bookworm-slim /usr/local /usr/local
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
# The copied image ships corepack shims (yarn etc.) that may dangle; clear
# them and enable only pnpm — the one manager DeerFlow's frontend needs.
RUN rm -f /usr/local/bin/yarn /usr/local/bin/yarnpkg /usr/local/bin/pnpm /usr/local/bin/pnpx \
    && corepack enable pnpm \
    && pnpm --version

# ---- OpenSandbox: server + Python SDK + osb CLI ----
# Pinned to the latest stable release (1.1.1) for reproducible classroom
# builds; the execd/egress images in opensandbox-server.toml are the
# versions that release pairs with.
RUN pip install opensandbox-server==1.1.1 opensandbox==1.1.1 opensandbox-cli==1.1.1

# ---- DeerFlow: backend (with the opensandbox extra) + pre-built frontend ----
# UV_EXTRAS keeps the extra across DeerFlow's own `uv sync --locked` runs
# (scripts/serve.sh re-syncs on every start); without it the provider import
# disappears after the first restart.
# Each step is its own layer with a cache mount, so rebuilds after config or
# script changes skip the slow dependency installs; only a DEERFLOW_REF bump
# re-runs them.
RUN git clone --depth 1 --branch "${DEERFLOW_REF}" "${DEERFLOW_REPO}" /opt/deer-flow

RUN --mount=type=cache,target=/root/.cache/uv \
    cd /opt/deer-flow/backend \
    && uv sync --all-packages --extra opensandbox

RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    cd /opt/deer-flow/frontend \
    && pnpm install --frozen-lockfile

# Slowest step: Next.js production build (reused at runtime via next start).
RUN cd /opt/deer-flow/frontend && pnpm run build
ENV PATH="/opt/deer-flow/backend/.venv/bin:${PATH}" \
    UV_EXTRAS=opensandbox
WORKDIR /opt/deer-flow

# ---- OpenSandbox checkout: the model-free example script + example docs ----
RUN git clone --depth 1 "${OPENSANDBOX_REPO}" /opt/OpenSandbox

# ---- Server config (container topology: mounted socket + host-gateway) ----
COPY opensandbox-server.toml /etc/opensandbox/config.toml
# Same teaching key as server.api_key in the config above; the provider's
# `api_key` config falls back to this env var.
ENV SANDBOX_CONFIG_PATH=/etc/opensandbox/config.toml \
    OPEN_SANDBOX_API_KEY=deerflow-lab-key

# ---- Helper scripts ----
COPY enable-opensandbox /usr/local/bin/enable-opensandbox
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/enable-opensandbox /usr/local/bin/entrypoint.sh

# 8080: OpenSandbox server API; 2026: DeerFlow Web UI (nginx entry point).
EXPOSE 8080 2026
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s \
  CMD curl -sf http://127.0.0.1:8080/health

ENTRYPOINT ["entrypoint.sh"]
