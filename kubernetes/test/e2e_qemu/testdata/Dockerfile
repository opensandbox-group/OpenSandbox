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

FROM golang:1.24-bookworm AS guest-builder

WORKDIR /src
COPY guest/ /src/
RUN CGO_ENABLED=0 GO111MODULE=off GOOS=linux GOARCH=amd64 go test ./...
RUN CGO_ENABLED=0 GO111MODULE=off GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/init /src/main.go

FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      busybox-static \
      cpio \
      curl \
      gzip \
      linux-image-virtual \
      qemu-system-x86 \
      qemu-utils \
      tini && \
    rm -rf /var/lib/apt/lists/*

COPY --from=guest-builder /out/init /build/initramfs/init

RUN set -eux; \
    mkdir -p /build/initramfs/dev /build/initramfs/proc /build/initramfs/sys /build/initramfs/tmp /vm; \
    kernel="$(find /boot -maxdepth 1 -name 'vmlinuz-*' -type f | sort -V | tail -n 1)"; \
    test -n "$kernel"; \
    kernel_version="${kernel##*/vmlinuz-}"; \
    cp "$kernel" /vm/vmlinuz; \
    mkdir -p /build/initramfs/bin /build/initramfs/lib/modules; \
    cp /bin/busybox /build/initramfs/bin/busybox; \
    cp -a "/lib/modules/$kernel_version" /build/initramfs/lib/modules/; \
    cd /build/initramfs; \
    find . -print0 | cpio --null -o --format=newc | gzip -9 > /vm/initramfs.cpio.gz; \
    qemu-img create -f qcow2 /vm/state.qcow2 4G; \
    rm -rf /build/initramfs /boot/*

COPY entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
