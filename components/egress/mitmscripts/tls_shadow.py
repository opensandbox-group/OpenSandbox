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

"""Request-weighted shadow projection, not a TLS admission decision.

Only consumes the validated snapshot already obtained for this HTTP flow.
No IPC, retained vault, subject cache, or flow mutation is permitted here.
"""

from __future__ import annotations

from host_selectors import parse_canonical


def project(
    sni: str | None, bindings: list[dict] | None, *, lookup_failed: bool = False
) -> str:
    """Return one bounded outcome without retaining or exposing request data."""
    if lookup_failed:
        return "lookup_failed"
    if not sni:
        return "missing_sni"
    if not sni.isascii():
        return "invalid_sni"
    try:
        host = parse_canonical(sni.lower().removesuffix("."))
    except ValueError:
        return "invalid_sni"
    if host.wildcard:
        return "invalid_sni"
    if bindings is None:
        return "no_vault"
    matched = False
    try:
        # Validate all eligible selectors, even if an earlier selector matches.
        for binding in bindings:
            match = binding["match"]
            if "https" not in match["schemes"]:
                continue
            for text in match["hosts"]:
                if text.startswith("*.") and len(text[2:]) > 251:
                    # Legacy Vault accepts 252/253-byte suffixes. A proper
                    # subdomain would exceed the 253-byte DNS limit, so this
                    # selector has no members. Still validate the suffix:
                    # malformed or oversized hosts must remain unavailable.
                    suffix = parse_canonical(text[2:])
                    if suffix.wildcard:
                        return "invalid_snapshot"
                    continue
                selector = parse_canonical(text)
                matched = selector.matches(host.text) or matched
    except (KeyError, TypeError, ValueError, AttributeError):
        return "invalid_snapshot"
    return "binding_host" if matched else "no_binding_host"
