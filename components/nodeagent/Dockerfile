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

FROM --platform=$BUILDPLATFORM golang:1.25.9-bookworm AS builder

WORKDIR /workspace
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG GOFLAGS=
ARG LDFLAGS=

COPY components/internal/go.mod components/internal/go.sum ./components/internal/
COPY components/nodeagent/go.mod components/nodeagent/go.sum ./components/nodeagent/
RUN cd components/internal && go mod download
RUN cd components/nodeagent && go mod download
COPY components/internal ./components/internal
COPY components/nodeagent ./components/nodeagent

WORKDIR /workspace/components/nodeagent
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
      -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
      -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
      -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/nodeagent . && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
      -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
      -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
      -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /out/nodeagent-oss-cleanup ./cmd/oss-cleanup

FROM debian:bookworm-slim
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/nodeagent /usr/local/bin/nodeagent
COPY --from=builder /out/nodeagent-oss-cleanup /usr/local/bin/nodeagent-oss-cleanup
ENTRYPOINT ["/usr/local/bin/nodeagent"]
