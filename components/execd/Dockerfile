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

FROM golang:1.25.9 AS builder

WORKDIR /build

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

# Prepare local modules to satisfy replace directives.
COPY components/internal/go.mod components/internal/go.sum ./components/internal/
COPY components/execd/go.mod components/execd/go.sum ./components/execd/

# Download deps with only mod files for better caching.
RUN cd components/internal && go mod download
RUN cd components/execd && go mod download

# Copy sources.
COPY components/internal ./components/internal
COPY components/execd ./components/execd

WORKDIR /build/components/execd

# Build the opensandbox-supervisor binary from the internal module.
RUN cd /build/components/internal && \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /build/opensandbox-supervisor ./cmd/supervisor

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
    -o /build/execd ./main.go

RUN CGO_ENABLED=0 GOOS=windows go build ${GOFLAGS} -trimpath -buildvcs=false \
    -ldflags "${LDFLAGS} -buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /build/execd.exe ./main.go

# Build static bubblewrap with musl.
FROM alpine:latest AS bwrap-builder
RUN apk add --no-cache git musl-dev meson ninja gcc libcap-dev libcap-static pkgconfig bash
COPY components/execd/native/session-gate.c /build/session-gate.c
RUN gcc -Os -static -s -Wall -Wextra -Werror \
    -o /build/opensandbox-session-gate /build/session-gate.c
COPY components/execd/native/launcher.c /build/launcher.c
RUN gcc -Os -static -s -Wall -Wextra -Werror \
    -o /build/opensandbox-launcher /build/launcher.c
RUN git clone --depth 1 --branch v0.11.2 \
    https://github.com/containers/bubblewrap /build/bwrap
WORKDIR /build/bwrap
RUN rm /usr/lib/libcap.so /usr/lib/libcap.so.2 && \
    meson setup builddir \
    -Dprefix=/usr \
    -Dman=disabled \
    -Dtests=false \
    -Dsupport_setuid=false \
    -Ddefault_library=static \
    -Dc_link_args='-static' && \
    ninja -C builddir && \
    cp builddir/bwrap /build/bwrap/bwrap

# execd-ebpf: observation variant binary (OSEP-0018 §5). The default image
# below carries both the minimal control-plane binary and this variant; who
# runs which is decided at launch time (bootstrap.sh honors the EXECD env,
# defaulting to /opt/opensandbox/execd). The variant attaches
# exec/connect/privilege audit hooks and needs CAP_BPF + CAP_PERFMON in the
# container ceiling.
#
# The CO-RE audit bytecode is REGENERATED here at image build time with
# bpf2go. prog/audit_types.h declares only the kernel members the programs
# touch (resolved by name against the target kernel BTF at load time), so
# no per-arch vmlinux.h or host kernel BTF is needed — this stage is fully
# hermetic and cross-compiles for every TARGETARCH.
FROM golang:1.25.9 AS ebpf-builder
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends clang
WORKDIR /build
COPY components/internal/go.mod components/internal/go.sum ./components/internal/
COPY components/execd/go.mod components/execd/go.sum ./components/execd/
RUN cd components/internal && go mod download
RUN cd components/execd && go mod download
COPY components/internal ./components/internal
COPY components/execd ./components/execd
WORKDIR /build/components/execd
# Generate the CO-RE bytecode for this TARGETARCH (bpf2go embeds it into
# audit_bpf_<arch>.go, picked up by the Go build tags). clang's bpf target
# only — no host kernel BTF.
RUN go run github.com/cilium/ebpf/cmd/bpf2go@v0.16.0 \
       -cc clang -no-strip \
       -cflags "-Ipkg/ebpf/prog" \
       -target "${TARGETARCH}" \
       -go-package ebpf -output-dir pkg/ebpf \
       audit pkg/ebpf/prog/audit.bpf.c
# cilium/ebpf is pure Go, so the variant builds fully static.
RUN CGO_ENABLED=0 go build -tags ebpf -trimpath -buildvcs=false \
    -ldflags "-buildid= -B none \
              -X 'github.com/alibaba/opensandbox/internal/version.Version=${VERSION}' \
              -X 'github.com/alibaba/opensandbox/internal/version.BuildTime=${BUILD_TIME}' \
              -X 'github.com/alibaba/opensandbox/internal/version.GitCommit=${GIT_COMMIT}'" \
    -o /build/execd-ebpf ./main.go

FROM alpine:latest

COPY --from=bwrap-builder --chown=0:0 /build/bwrap/bwrap /usr/local/bin/bwrap
COPY --from=bwrap-builder --chown=0:0 /build/opensandbox-session-gate /usr/local/libexec/opensandbox-session-gate
COPY --from=bwrap-builder --chown=0:0 /build/opensandbox-session-gate /opt/opensandbox/opensandbox-session-gate
COPY --from=bwrap-builder --chown=0:0 /build/opensandbox-launcher /usr/local/libexec/opensandbox-launcher
COPY --from=bwrap-builder --chown=0:0 /build/opensandbox-launcher /opt/opensandbox/opensandbox-launcher
COPY --from=builder /build/execd .
COPY --from=builder /build/execd.exe ./execd.exe
COPY --from=builder /build/opensandbox-supervisor ./opensandbox-supervisor
COPY --from=ebpf-builder /build/execd-ebpf ./execd-ebpf
COPY components/execd/bootstrap.sh ./bootstrap.sh
COPY components/execd/install.bat ./install.bat

# Defensive: normalize execd-related file modes regardless of source checkout
# permissions or the builder's umask.
RUN chmod 0755 \
    ./execd \
    ./execd.exe \
    ./execd-ebpf \
    ./opensandbox-supervisor \
    ./bootstrap.sh \
    ./install.bat \
    /usr/local/bin/bwrap \
    /usr/local/libexec/opensandbox-session-gate \
    /opt/opensandbox/opensandbox-session-gate \
    /usr/local/libexec/opensandbox-launcher \
    /opt/opensandbox/opensandbox-launcher

ENTRYPOINT ["./execd"]
