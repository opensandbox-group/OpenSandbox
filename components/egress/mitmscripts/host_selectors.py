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

"""OSEP-0023 canonical snapshot selectors, also used by shadow observations.

Go owns Unicode/IDNA normalization at the control-plane input boundary. This
module validates the resulting ASCII structure, never reinterpreting U-labels
with Python's different IDNA codec. Wire SNI may vary in case or one root dot.
"""

from __future__ import annotations

import ipaddress
import re
from typing import NamedTuple

_LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?")


def _valid_host(host: str) -> bool:
    if len(host) > 253 or "." not in host:
        return False
    try:
        ipaddress.ip_address(host)
    except ValueError:
        pass
    else:
        return False
    return all(_LABEL.fullmatch(label) is not None for label in host.split("."))


class Selector(NamedTuple):
    """Immutable selector constructed with parse_canonical."""

    base: str
    wildcard: bool

    @property
    def text(self) -> str:
        return ("*." if self.wildcard else "") + self.base

    def matches(self, host: str) -> bool:
        """Match ASCII wire SNI; invalid names match nothing."""
        if not host.isascii():
            return False
        host = host.lower().removesuffix(".")
        if not _valid_host(host):
            return False
        if self.wildcard:
            return host.endswith("." + self.base)
        return host == self.base

    def overlaps(self, other: Selector) -> bool:
        """Test semantic intersection, including nested wildcard suffixes."""
        if not self.wildcard:
            return other.matches(self.base)
        if not other.wildcard:
            return self.matches(other.base)
        return (
            self.base == other.base
            or self.base.endswith("." + other.base)
            or other.base.endswith("." + self.base)
        )


def parse_canonical(text: str) -> Selector:
    """Read a Go-normalized snapshot selector; reject noncanonical structure."""
    wildcard = text.startswith("*.")
    base = text[2:] if wildcard else text
    if not _valid_host(base) or (wildcard and len(base) > 251):
        raise ValueError("invalid host selector")
    return Selector(base, wildcard)
