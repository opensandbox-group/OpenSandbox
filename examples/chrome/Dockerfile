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

FROM golang:1.25.4 AS builder

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /build/entrypoint main.go

#----------------------
# Use a base image with a minimal set of packages.
FROM debian:13-slim

#----------------------
# Install prerequisites, chromium, VNC, and X11 utilities.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      wget \
      xdg-utils \
      chromium \
      tigervnc-standalone-server \
      x11-utils; \
    rm -rf /var/lib/apt/lists/*

# Create a non-root user to run Chrome.
RUN groupadd -r chrome && useradd -r -g chrome -G audio,video chrome \
    && mkdir -p /home/chrome/Downloads && chown -R chrome:chrome /home/chrome

# Precreate X11 stuff
RUN mkdir -p /tmp/.X11-unix
RUN chmod 1777 /tmp/.X11-unix

COPY --chmod=a+rx chrome.sh /chrome.sh
COPY --from=builder --chmod=a+rx /build/entrypoint /entrypoint

WORKDIR /home/chrome

USER chrome


ENTRYPOINT [ "/entrypoint" ]
