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

# Build the manager binary
# Go >=1.25.10 is required to pick up Go stdlib security fixes; the 1.24 branch is
# EOL for them. Pinned to an explicit patch tag + digest (the digest resolves to
# 1.25.10); dependabot keeps tag and digest in lockstep.
FROM golang:1.25.10@sha256:c138bff780910acf4254ab3a6f7ff0f64bbd841f27bd82bfa986fe122c109538 AS builder
ARG TARGETOS
ARG TARGETARCH
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

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN GOPROXY=https://goproxy.cn,direct go mod download

# Copy the go source
COPY cmd/ cmd/
COPY apis/ apis/
COPY pkg/ pkg/
COPY internal/ internal/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN echo "Building for $TARGETOS/$TARGETARCH"
ARG PACKAGE=./cmd/controller
ARG COMMIT_ID=unknown
ARG BUILD_DATE=unknown
RUN if [ -n "${CC}" ]; then export CC; fi; \
    if [ -n "${CXX}" ]; then export CXX; fi; \
    export CGO_ENABLED="${CGO_ENABLED}" GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
           CGO_CFLAGS="${CGO_CFLAGS:-${CFLAGS}}" \
           CGO_CXXFLAGS="${CGO_CXXFLAGS:-${CXXFLAGS}}" \
           CGO_LDFLAGS="${CGO_LDFLAGS}"; \
    go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none -X main.commitID=${COMMIT_ID} -X main.buildDate=${BUILD_DATE}" \
    -o server ${PACKAGE}

# Minimal runtime instead of the full golang toolchain image (which shipped the
# entire Debian 13 userland + build tools -> hundreds of OS-package CVEs).
# The task-executor needs nsenter + /bin/sh; the controller shells out to nothing.
# distroless is unusable here because it has neither nsenter nor a shell.
# Pinned by digest (dependabot keeps tag+digest current).
FROM debian:13-slim@sha256:b6e2a152f22a40ff69d92cb397223c906017e1391a73c952b588e51af8883bf8
ARG USERID=65532

# Install ONLY the runtime deps that are not already in the slim base. OS security
# fixes come from the pinned base digest (kept current by Dependabot), not from an
# `apt-get upgrade` which would make builds non-reproducible. In Debian trixie
# nsenter lives in util-linux-extra (util-linux merely Recommends it), so install
# util-linux-extra explicitly.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         util-linux-extra \
         ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Fail the build early if the runtime deps are missing.
RUN command -v nsenter >/dev/null && [ -x /bin/sh ]

WORKDIR /workspace
COPY --from=builder /workspace/server .
USER $USERID
ENTRYPOINT ["/workspace/server"]
