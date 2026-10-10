# Copyright 2026 The OpenSandbox Authors
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

# Security-validation variant:
# - Keep the upstream build context as server/
# - Pin the Python base image by digest
# - Apply Debian security updates explicitly
# - Update system Python packaging tooling so vulnerable vendored copies
#   of jaraco.context/wheel are not retained in the final image
# - Keep the existing application/runtime layout unchanged

ARG SETUPTOOLS_SCM_PRETEND_VERSION
ENV SETUPTOOLS_SCM_PRETEND_VERSION=$SETUPTOOLS_SCM_PRETEND_VERSION

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    UV_PROJECT_ENV=/app/.venv \
    UV_LINK_MODE=copy

WORKDIR /app

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        curl \
        ca-certificates \
    && apt-get install -y --only-upgrade \
        gzip \
        libpcre2-8-0 \
        libsqlite3-0 \
        perl-base \
    && rm -rf /var/lib/apt/lists/*

RUN curl -LsSf https://astral.sh/uv/install.sh | sh

ENV PATH="/root/.local/bin:/root/.cargo/bin:${PATH}"

COPY pyproject.toml uv.lock ./

RUN uv sync --frozen --no-dev --no-install-project

# Keep build tooling current enough to avoid shipping the vulnerable
# setuptools-vendored jaraco.context/wheel versions detected by Trivy.
RUN uv pip install --python /app/.venv/bin/python --upgrade \
    "setuptools>=82.0.1" \
    "wheel>=0.46.2" \
    "jaraco.context>=6.1.0"

COPY opensandbox_server ./opensandbox_server

COPY LICENSE README.md ./

RUN uv pip install --python /app/.venv/bin/python --no-deps --editable .

# Keep application environment packaging tools current as well.
RUN uv pip install --python /app/.venv/bin/python --upgrade \
        "setuptools>=82.0.1" \
        "wheel>=0.46.2"

FROM ${PYTHON_BASE} AS runtime

ENV PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    UV_PROJECT_ENV=/app/.venv \
    PATH="/app/.venv/bin:${PATH}" \
    SANDBOX_CONFIG_PATH=/etc/opensandbox/config.toml

WORKDIR /app

# Apply Debian security updates in the final runtime image.
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
    && apt-get install -y --only-upgrade \
        gzip \
        libpcre2-8-0 \
        libsqlite3-0 \
        perl-base \
    && rm -rf /var/lib/apt/lists/*

# Refresh system packaging tooling. The base image contains setuptools with
# vendored jaraco.context/wheel copies that were reported by Trivy.
RUN python -m pip install --no-cache-dir --upgrade \
        "setuptools>=82.0.1" \
        "wheel>=0.46.2" \
    && python - <<'PY'
import importlib.metadata as md
for name in ("setuptools", "wheel"):
    print(f"{name}: {md.version(name)}")
PY

COPY --from=builder /app/.venv /app/.venv
COPY --from=builder /app/opensandbox_server /app/opensandbox_server

# Use the Docker runtime configuration for this Docker deployment.
# The Kubernetes example is intentionally not used by the Docker image.
COPY --from=builder /app/opensandbox_server/examples/example.config.toml /etc/opensandbox/config.toml
COPY --from=builder /app/opensandbox_server/examples/example.config.zh.toml /etc/opensandbox/config.zh.toml

# Containers must bind the server to all interfaces so the published Docker
# port is reachable from the host/load balancer. The upstream Docker example
# intentionally uses 127.0.0.1 for local standalone execution.
RUN sed -i 's/^host = "127\.0\.0\.1"$/host = "0.0.0.0"/' /etc/opensandbox/config.toml     && grep -E '^(host|port) = ' /etc/opensandbox/config.toml
COPY --from=builder /app/opensandbox_server/examples/example.batchsandbox-template.yaml /etc/opensandbox/example.batchsandbox-template.yaml
COPY --from=builder /app/opensandbox_server/examples/e2e.batchsandbox-template.yaml /etc/opensandbox/e2e.batchsandbox-template.yaml

# Print package versions during the image build so the security pipeline has
# direct evidence of the remediation.
#
# This is diagnostic only: a package that is not installed must not make the
# Docker build fail. Trivy remains the security gate in CI.
RUN echo "===== Debian package versions =====" \
    && for pkg in perl-base gzip libpcre2-8-0 libsqlite3-0; do \
        echo "--- $pkg ---"; \
        dpkg-query -W -f='${Package} ${Version}\n' "$pkg" 2>/dev/null || echo "$pkg: NOT INSTALLED"; \
    done \
    && echo "===== Python packaging versions =====" \
    && python -m pip show setuptools 2>/dev/null || echo "setuptools: NOT FOUND" \
    && python -m pip show wheel 2>/dev/null || echo "wheel: NOT FOUND" \
    && echo "===== Python packaging metadata =====" \
    && python -c 'import importlib.metadata as md; [print(f"{name}: {md.version(name)}") for name in ("setuptools", "wheel")]' \
    && echo "===== Vendored packaging metadata =====" \
    && find /usr/local/lib/python3.10/site-packages \
        -maxdepth 4 \
        \( -name 'jaraco.context-*.dist-info' -o -name 'wheel-*.dist-info' \) \
        -print || true

EXPOSE 8080

ENTRYPOINT ["opensandbox-server"]

CMD ["--config", "/etc/opensandbox/config.toml"]
