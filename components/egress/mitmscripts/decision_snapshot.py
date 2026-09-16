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

"""Strict validator for the OSEP-0023 canonical decision payload.

The live addon imports this module only for a launcher-provided internal
revision session; current egress profiles do not supply one. Go owns
construction and host normalization; this validator verifies the exact
payload/envelope agreement before the revision receiver may stage those
immutable bytes.
"""

from __future__ import annotations

import hashlib
import json
import re
from typing import Any
from urllib.parse import quote, quote_plus

from host_selectors import parse_canonical
from revision_receiver import Snapshot

_FIELDS = {
    "version",
    "vaultRevision",
    "effectivePolicyEpoch",
    "interceptionMode",
    "state",
    "tlsBindingHostSelectors",
    "fullRenderedBindings",
    "redactions",
}
_MATCH_FIELDS = {"schemes", "hosts", "methods", "paths"}
_HEADER = re.compile(r"[A-Za-z0-9!#$%&'*+\-.^_`|~]+")
_PERCENT_ESCAPE = re.compile(r"%([0-9A-Fa-f]{2})")
_RESERVED_HEADERS = {
    "host",
    "content-length",
    "content-type",
    "transfer-encoding",
    "connection",
    "upgrade",
    "te",
    "trailer",
    "proxy-authorization",
    "proxy-authenticate",
    "forwarded",
    "x-forwarded-for",
    "x-forwarded-host",
    "x-forwarded-proto",
}


class DecisionSnapshotError(Exception):
    """A fixed rejection that never contains rendered credential data."""


class _Invalid(Exception):
    pass


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise _Invalid
        result[key] = value
    return result


def _decode(raw: bytes) -> dict[str, Any]:
    try:
        value = json.loads(
            raw,
            object_pairs_hook=_unique_object,
            parse_constant=lambda _value: (_ for _ in ()).throw(_Invalid()),
        )
    except (json.JSONDecodeError, UnicodeDecodeError, RecursionError, _Invalid):
        raise _Invalid from None
    if type(value) is not dict or set(value) != _FIELDS:
        raise _Invalid
    return value


def _strings(value: Any, valid: Any, *, empty: bool = False) -> list[str]:
    if type(value) is not list or not empty and not value:
        raise _Invalid
    seen = set()
    for item in value:
        if type(item) is not str or not valid(item) or item in seen:
            raise _Invalid
        seen.add(item)
    return value


def _host(text: str) -> bool:
    try:
        if text.startswith("*.") and len(text[2:]) > 251:
            suffix = parse_canonical(text[2:])
            return not suffix.wildcard and suffix.text == text[2:]
        return parse_canonical(text).text == text
    except (AttributeError, ValueError):
        return False


def _redaction_variants(value: str) -> set[str]:
    url_encoded = quote(value, safe="")
    form_encoded = quote_plus(value, safe="")
    variants = {
        value,
        url_encoded,
        _PERCENT_ESCAPE.sub(lambda match: f"%{match.group(1).lower()}", url_encoded),
        form_encoded,
        _PERCENT_ESCAPE.sub(lambda match: f"%{match.group(1).lower()}", form_encoded),
        _go_json_content(value),
        _go_ascii_json_content(value),
    }
    variants.discard("")
    return variants


def _go_json_content(value: str) -> str:
    encoded = json.dumps(value, ensure_ascii=False)[1:-1]
    return (
        encoded.replace("<", r"\u003c")
        .replace(">", r"\u003e")
        .replace("&", r"\u0026")
        .replace("\u2028", r"\u2028")
        .replace("\u2029", r"\u2029")
    )


def _go_ascii_json_content(value: str) -> str:
    encoded = []
    escapes = {
        "\\": r"\\",
        '"': r"\"",
        "\b": r"\b",
        "\f": r"\f",
        "\n": r"\n",
        "\r": r"\r",
        "\t": r"\t",
    }
    for character in value:
        if character in escapes:
            encoded.append(escapes[character])
            continue
        codepoint = ord(character)
        if codepoint < 0x20:
            encoded.append(f"\\u{codepoint:04x}")
        elif codepoint < 0x80:
            encoded.append(character)
        elif codepoint <= 0xFFFF:
            encoded.append(f"\\u{codepoint:04x}")
        else:
            codepoint -= 0x10000
            encoded.append(
                f"\\u{0xD800 + (codepoint >> 10):04x}"
                f"\\u{0xDC00 + (codepoint & 0x3FF):04x}"
            )
    return "".join(encoded)


def _binding(value: Any, redactions: set[str]) -> tuple[str, set[str]]:
    allowed = {"name", "match", "headers"}
    if type(value) is not dict or not (
        set(value) == allowed or set(value) == allowed | {"substitutions"}
    ):
        raise _Invalid
    name = value["name"]
    if type(name) is not str or not name or name != name.strip():
        raise _Invalid
    match = value["match"]
    if type(match) is not dict or set(match) != _MATCH_FIELDS:
        raise _Invalid
    schemes = _strings(match["schemes"], lambda item: item in {"http", "https"})
    hosts = _strings(match["hosts"], _host)
    _strings(match["methods"], lambda item: bool(item) and item == item.strip().upper())
    _strings(match["paths"], lambda item: item == item.strip() and item.startswith("/"))

    headers = value["headers"]
    if type(headers) is not list:
        raise _Invalid
    seen_headers = set()
    for header in headers:
        if type(header) is not dict or set(header) != {"name", "value"}:
            raise _Invalid
        header_name, header_value = header["name"], header["value"]
        key = header_name.lower() if type(header_name) is str else ""
        if (
            not key
            or _HEADER.fullmatch(header_name) is None
            or key in _RESERVED_HEADERS
            or key in seen_headers
            or type(header_value) is not str
            or header_value
            and header_value not in redactions
        ):
            raise _Invalid
        seen_headers.add(key)

    substitutions = value.get("substitutions", [])
    if type(substitutions) is not list:
        raise _Invalid
    for substitution in substitutions:
        if type(substitution) is not dict or set(substitution) != {
            "placeholder",
            "value",
            "in",
        }:
            raise _Invalid
        placeholder, rendered = substitution["placeholder"], substitution["value"]
        if type(placeholder) is not str or not placeholder or type(rendered) is not str:
            raise _Invalid
        _strings(
            substitution["in"],
            lambda item: item in {"path", "query", "header", "body"},
        )
        if (
            placeholder not in redactions
            or not _redaction_variants(rendered) <= redactions
        ):
            raise _Invalid

    selectors = set()
    if "https" in schemes:
        selectors.update(
            host
            for host in hosts
            if not (host.startswith("*.") and len(host[2:]) > 251)
        )
    return name, selectors


def _validate(snapshot: Snapshot) -> None:
    if (
        type(snapshot) is not Snapshot
        or hashlib.sha256(snapshot.payload).hexdigest() != snapshot.revision.digest
    ):
        raise _Invalid
    value = _decode(snapshot.payload)
    integer = lambda item: type(item) is int and item >= 0
    if (
        type(value["version"]) is not int
        or value["version"] != 1
        or not integer(value["vaultRevision"])
        or not integer(value["effectivePolicyEpoch"])
        or value["vaultRevision"] != snapshot.revision.vault_revision
        or value["effectivePolicyEpoch"] != snapshot.revision.policy_epoch
        or value["interceptionMode"] != "credential-bound"
    ):
        raise _Invalid

    redactions = _strings(value["redactions"], lambda item: bool(item), empty=True)
    if redactions != sorted(redactions, key=lambda item: (-len(item.encode()), item)):
        raise _Invalid
    bindings = value["fullRenderedBindings"]
    if type(bindings) is not list or not bindings and redactions:
        raise _Invalid
    names, derived = [], set()
    for binding in bindings:
        name, selectors = _binding(binding, set(redactions))
        names.append(name)
        derived.update(selectors)
    if names != sorted(names) or len(names) != len(set(names)):
        raise _Invalid

    selectors = _strings(value["tlsBindingHostSelectors"], _host, empty=True)
    if selectors != sorted(derived):
        raise _Invalid
    expected_state = "active" if bindings else "active-empty"
    if value["state"] != expected_state or bindings and value["vaultRevision"] == 0:
        raise _Invalid


def validate(snapshot: Snapshot) -> None:
    """Validate one immutable payload for use as a Receiver callback."""
    valid = True
    try:
        _validate(snapshot)
    except Exception:  # noqa: BLE001 - payload and rendered values are secret-bearing
        valid = False
    if not valid:
        raise DecisionSnapshotError("invalid credential decision snapshot")
