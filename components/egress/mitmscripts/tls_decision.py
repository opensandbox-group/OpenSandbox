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

"""Pure, credential-free TLS selection for acknowledged OSEP-0023 snapshots."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Literal

from decision_snapshot import validate
from host_selectors import Selector, parse_canonical
from revision_receiver import Revision, Snapshot
from static_passthrough import StaticPassthrough, validate_selectors

Generation = tuple[str, str]
Action = Literal["deny", "passthrough", "needs_registry"]
Reason = Literal[
    "unknown_identity",
    "ech",
    "no_sni",
    "invalid_sni",
    "static_ignore",
    "snapshot_missing",
    "generation_mismatch",
    "no_binding_host",
    "binding_host",
    "invalid_input",
]


class TLSDecisionError(Exception):
    """Fixed validation error that does not expose snapshot content."""


@dataclass(frozen=True, slots=True)
class TLSSelectorView:
    """Validated revision metadata and its credential-free TLS selectors."""

    revision: Revision
    selectors: tuple[Selector, ...]


@dataclass(frozen=True, slots=True)
class TLSDecision:
    """Closed classification output; it never carries a host or credential."""

    action: Action
    reason: Reason


def compile_view(snapshot: Snapshot) -> TLSSelectorView:
    """Strictly validate a real snapshot, then discard all payload data."""
    valid = True
    try:
        validate(snapshot)
    except Exception:  # noqa: BLE001 - snapshot contents may contain credentials
        valid = False
    if not valid:
        raise TLSDecisionError("invalid TLS decision snapshot")

    value = json.loads(snapshot.payload)
    revision = snapshot.revision
    return TLSSelectorView(
        revision=revision,
        selectors=tuple(
            parse_canonical(text) for text in value["tlsBindingHostSelectors"]
        ),
    )


def _known_identity(identity: Generation | None) -> bool:
    return (
        type(identity) is tuple
        and len(identity) == 2
        and all(type(generation) is str and bool(generation) for generation in identity)
    )


def _valid_sni(sni: object) -> bool:
    if type(sni) is not str or not sni.isascii():
        return False
    normalized = sni.lower().removesuffix(".")
    if not normalized:
        return False
    try:
        return parse_canonical(normalized).text == normalized
    except ValueError:
        return False


def _valid_static_passthrough(value: object) -> bool:
    try:
        validate_selectors(value)
    except ValueError:
        return False
    return True


def _decision(action: Action, reason: Reason) -> TLSDecision:
    return TLSDecision(action, reason)


def _classify(
    *,
    identity: Generation | None,
    sni: str | None,
    ech_hidden: bool,
    static_passthrough: tuple[Selector, ...],
    view: TLSSelectorView | None,
    destination_port: int | None = None,
    _static_config: StaticPassthrough | None = None,
) -> TLSDecision:
    """Internal selection; only the startup owner supplies a compiled policy."""
    if not _known_identity(identity):
        return _decision("deny", "unknown_identity")
    if type(ech_hidden) is not bool:
        return _decision("deny", "invalid_input")
    if ech_hidden:
        return _decision("passthrough", "ech")
    if sni is None:
        return _decision("passthrough", "no_sni")
    if not _valid_sni(sni):
        return _decision("deny", "invalid_sni")
    startup_tuple = (
        type(_static_config) is StaticPassthrough
        and static_passthrough is _static_config.selectors
    )
    if not startup_tuple and not _valid_static_passthrough(static_passthrough):
        return _decision("deny", "invalid_input")
    if any(selector.matches(sni) for selector in static_passthrough):
        if type(destination_port) is not int or not 1 <= destination_port <= 65535:
            return _decision("deny", "invalid_input")
        if destination_port == 443:
            return _decision("passthrough", "static_ignore")
    if type(view) is not TLSSelectorView:
        return _decision("deny", "snapshot_missing")
    if identity != (
        view.revision.control_generation,
        view.revision.subject_generation,
    ):
        return _decision("deny", "generation_mismatch")
    if not any(selector.matches(sni) for selector in view.selectors):
        return _decision("passthrough", "no_binding_host")
    return _decision("needs_registry", "binding_host")


def classify(
    *,
    identity: Generation | None,
    sni: str | None,
    ech_hidden: bool,
    static_passthrough: tuple[Selector, ...],
    view: TLSSelectorView | None,
    destination_port: int | None = None,
) -> TLSDecision:
    """Validate caller-owned static tuples in the same ClientHello order."""
    return _classify(
        identity=identity, sni=sni, ech_hidden=ech_hidden,
        static_passthrough=static_passthrough, view=view,
        destination_port=destination_port,
    )
