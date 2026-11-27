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

FROM golang:1.26.11-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG GOFLAGS=
ARG GOPROXY=https://proxy.golang.org,direct
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
RUN apk add --no-cache git
COPY go.mod go.mod
COPY go.sum go.sum
RUN GOPROXY="${GOPROXY}" go mod download

COPY cmd/ cmd/
COPY apis/ apis/
COPY pkg/ pkg/
COPY internal/ internal/

# Empty TARGETARCH uses the builder's architecture rather than forcing a different one.
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

# Use golang image as base to ensure nsenter (util-linux) is available
# distroless does not contain shell or nsenter
FROM golang:1.26.11-alpine
ARG USERID=65532
WORKDIR /workspace
COPY --from=builder /workspace/server .
USER $USERID
ENTRYPOINT ["/workspace/server"]
