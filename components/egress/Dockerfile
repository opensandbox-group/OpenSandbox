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

FROM golang:1.26.5-alpine3.23 AS builder

WORKDIR /workspace

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG GOFLAGS=
ARG LDFLAGS=
ARG CGO_ENABLED=0
ARG CC=
ARG CXX=
ARG CFLAGS=
ARG CXXFLAGS=
ARG CGO_CFLAGS=
ARG CGO_CXXFLAGS=
ARG CGO_LDFLAGS=

# Copy only go mod/sum first for better caching
COPY components/egress/go.mod components/egress/go.sum ./components/egress/
# Bring internal module so replace ../internal works during download/build
COPY components/internal ./components/internal

WORKDIR /workspace/components/egress

# Static-ish build (no cgo by default) to simplify runtime deps.
RUN go mod download

# Pre-download internal-module deps for the supervisor build below.
RUN cd /workspace/components/internal && go mod download

# Copy the rest of the egress sources
COPY components/egress ./
RUN if [ -n "${CC}" ]; then export CC; fi; \
    if [ -n "${CXX}" ]; then export CXX; fi; \
    export CGO_ENABLED="${CGO_ENABLED}" \
           CGO_CFLAGS="${CGO_CFLAGS:-${CFLAGS}}" \
           CGO_CXXFLAGS="${CGO_CXXFLAGS:-${CXXFLAGS}}" \
           CGO_LDFLAGS="${CGO_LDFLAGS}"; \
    go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/egress .

# Build the opensandbox-supervisor binary from the internal module.
# Installed alongside /egress so a future ENTRYPOINT switch can wrap egress
# without changing this stage again.
RUN cd /workspace/components/internal && \
    if [ -n "${CC}" ]; then export CC; fi; \
    if [ -n "${CXX}" ]; then export CXX; fi; \
    export CGO_ENABLED="${CGO_ENABLED}" \
           CGO_CFLAGS="${CGO_CFLAGS:-${CFLAGS}}" \
           CGO_CXXFLAGS="${CGO_CXXFLAGS:-${CXXFLAGS}}" \
           CGO_LDFLAGS="${CGO_LDFLAGS}"; \
    go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/opensandbox-supervisor ./cmd/supervisor

FROM cgr.dev/chainguard/wolfi-base@sha256:07e60ff6586b56f03c625e27b604f9f7d29498fef32f099f6560f0d207b4a056

# Keep only the packages required by the egress runtime. The prior diagnostic
# toolbelt materially expanded the image's vulnerability surface and is not
# used by the entrypoint or enforcement code.
RUN apk add --no-cache \
        ca-certificates \
        iproute2 \
        iptables \
        nftables \
        procps \
        py3.12-pip \
        python-3.12 \
        sudo

# Python mitmproxy (transparent mode): mitmdump runs as user mitmproxy; iptables skips this uid.
# /var/lib/mitmproxy is mitm's home, used as the confdir (CA + config.yaml live under .mitmproxy/).
RUN addgroup -S -g 10042 mitmproxy \
    && adduser -S -D -H -u 10042 -G mitmproxy -h /var/lib/mitmproxy -s /sbin/nologin mitmproxy \
    && mkdir -p /var/lib/mitmproxy/.mitmproxy \
    && chown -R mitmproxy:mitmproxy /var/lib/mitmproxy \
    && pip3.12 install --no-cache-dir 'mitmproxy==12.2.3' \
    && (command -v mitmdump && mitmdump --version) \
    && rm -rf /root/.cache \
    && mkdir -p /var/egress/mitmscripts

# Static mitmproxy options (mode, listen_host, connection_strategy, stream_large_bodies,
# http2, ignore_hosts, ssl_verify_upstream_trusted_confdir). mitmdump auto-loads
# config.yaml from its confdir. Dynamic per-deployment options stay env-driven and
# are applied as --set by launch.go (which overrides values declared here).
COPY components/egress/mitmproxy/config.yaml /var/lib/mitmproxy/.mitmproxy/config.yaml
RUN chown mitmproxy:mitmproxy /var/lib/mitmproxy/.mitmproxy/config.yaml \
    && chmod 0644 /var/lib/mitmproxy/.mitmproxy/config.yaml

# All egress runtime artifacts live under one directory to keep paths grouped.
COPY --from=builder /out/egress /opt/opensandbox-egress/egress
COPY --from=builder /out/opensandbox-supervisor /opt/opensandbox-egress/supervisor
# Pre-start hook: reap any mitmdump left over from a previous crashed
# egress, and remove stale DNS-redirect iptables rules + opensandbox_dns_redirect
# nft tables left pointing at a dead proxy. The `inet opensandbox` policy table
# is intentionally NOT touched — the nftables manager prepends its own delete
# and recreates the table on every ApplyStatic, so it is idempotent.
COPY components/egress/scripts/cleanup.sh /opt/opensandbox-egress/cleanup.sh
RUN chmod 0755 /opt/opensandbox-egress/cleanup.sh \
            /opt/opensandbox-egress/egress \
            /opt/opensandbox-egress/supervisor \
    && ln -s /opt/opensandbox-egress/egress /egress

COPY components/egress/mitmscripts /var/egress/mitmscripts
# The mitmdump addon is loaded by the mitmproxy user (uid 10042): normalize
# permissions so a restrictive build umask (e.g. 027) can never leave the
# scripts unreadable to it.
RUN chmod -R a+rX /var/egress/mitmscripts

# Supervisor wraps the egress binary: restarts on crash with backoff and
# forwards SIGTERM gracefully. The cleanup hook runs only as pre-start; the
# next egress process re-installs all redirect rules itself after startup,
# so there is nothing to run on post-exit.
# Expects OPENSANDBOX_NETWORK_POLICY env at runtime.
ENTRYPOINT ["/opt/opensandbox-egress/supervisor", \
            "--pre-start=/opt/opensandbox-egress/cleanup.sh", \
            "--name=egress", \
            "--grace-period=20s", \
            "--", \
            "/opt/opensandbox-egress/egress"]
