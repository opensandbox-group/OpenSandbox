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

"""Strict, immutable canonical configuration delivered by the Go launcher."""
from __future__ import annotations

import json
from dataclasses import dataclass
from host_selectors import Selector, parse_canonical

ENV = "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"
MAX_BYTES = 65536
MAX_SELECTORS = 256
ERROR = "credential proxy: invalid " + ENV
NATIVE_OPTIONS_ERROR = "credential proxy: live ignore_hosts/allow_hosts must be empty; use " + ENV


def validate_selectors(value: object) -> tuple[Selector, ...]:
    """Reject mutable containers and forged selectors; preserve tuple identity."""
    try:
        if type(value) is not tuple or len(value) > MAX_SELECTORS:
            raise ValueError
        texts = []
        for selector in value:
            if (type(selector) is not Selector or type(selector.base) is not str
                    or type(selector.wildcard) is not bool
                    or not selector.text.isascii()
                    or parse_canonical(selector.text) != selector):
                raise ValueError
            texts.append(selector.text)
        if texts != sorted(set(texts)):
            raise ValueError
        # Canonical selector characters never require JSON escaping.
        encoded_size = 2 + sum(len(text) + 2 for text in texts) + max(len(texts) - 1, 0)
        if encoded_size > MAX_BYTES:
            raise ValueError
    except (AttributeError, TypeError, ValueError):
        raise ValueError(ERROR) from None
    return value


@dataclass(frozen=True, slots=True)
class StaticPassthrough:
    """One startup-owned, validated tuple; not an untrusted classifier input."""

    selectors: tuple[Selector, ...]

    def __post_init__(self) -> None:
        validate_selectors(self.selectors)


def parse_handoff(raw: str | None) -> tuple[Selector, ...]:
    """Missing child configuration is an error, including for an empty policy."""
    try:
        if type(raw) is not str or len(raw.encode("utf-8")) > MAX_BYTES:
            raise ValueError
        values = json.loads(raw)
        if type(values) is not list or len(values) > MAX_SELECTORS:
            raise ValueError
        if any(type(text) is not str or not text.isascii() for text in values):
            raise ValueError
        return validate_selectors(tuple(parse_canonical(text) for text in values))
    except (TypeError, ValueError, UnicodeError, RecursionError):
        raise ValueError(ERROR) from None


def destination_port(address: object) -> int | None:
    """Read the original destination, never Host, SNI, or connected peername."""
    if (type(address) is tuple and len(address) == 2
            and type(address[0]) is str and bool(address[0])
            and type(address[1]) is int and 1 <= address[1] <= 65535):
        return address[1]
    return None
