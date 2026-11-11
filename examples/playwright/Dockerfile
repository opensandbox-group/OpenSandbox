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
    python3-venv \
    wget \
    ca-certificates \
    curl \
    git \
    nodejs \
    npm \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

#----------------------
# Create a non-root user and browser cache dir early (needed before chown)
RUN groupadd -r playwright && useradd -r -g playwright playwright \
    && mkdir -p /home/playwright /ms-playwright \
    && chown -R playwright:playwright /home/playwright /ms-playwright

#----------------------
# Install Playwright and browser binaries
# Use an isolated venv to avoid PEP 668 issues
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

RUN python3 -m venv /venv \
    && /venv/bin/pip install --no-cache-dir --upgrade pip \
    && /venv/bin/pip install --no-cache-dir playwright \
    && npm install -g @playwright/mcp \
    && /venv/bin/playwright install --with-deps chromium

ENV PATH="/venv/bin:${PATH}"

#----------------------
# Configure user, etc

WORKDIR /home/playwright

USER playwright

# Default to bash
CMD ["bash"]
