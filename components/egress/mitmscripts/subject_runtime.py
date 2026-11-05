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

"""Launcher-selected shared subject receiver; installation only, no TLS hooks."""

from __future__ import annotations

from contextlib import suppress
import os
import sys
from typing import Any, Mapping, NoReturn

from revision_ipc import _valid_token
from subject_revision import SubjectRevisionOwner, valid_identity
from subject_revision_ipc import Server

_PREFIX = "OPENSANDBOX_EGRESS_SUBJECT_REVISION_"
_FIELDS = {
    "MODE", "SOCKET", "TOKEN", "CONTROL_GENERATION", "MAX_SUBJECTS",
    "MAX_SNAPSHOT_BYTES", "RETAINED_PAYLOAD_BUDGET",
}
_ERROR = "credential proxy: invalid revision runtime configuration"
_owner: SubjectRevisionOwner | None = None
_server: Server | None = None


def _fatal() -> NoReturn:
    # Script watcher exceptions can otherwise leave a listener without its addon.
    raise SystemExit(_ERROR) from None


def _configuration(environ: Mapping[str, str]) -> dict[str, Any]:
    values = {name[len(_PREFIX):]: value for name, value in environ.items()
              if name.startswith(_PREFIX)}
    if set(values) != _FIELDS or values["MODE"] != "subject-v2":
        _fatal()
    if (any(name.startswith("OPENSANDBOX_EGRESS_REVISION_") for name in environ)
            or not os.path.isabs(values["SOCKET"])
            or "\0" in values["SOCKET"] or len(os.fsencode(values["SOCKET"])) > 103
            or not _valid_token(values["TOKEN"])
            or not valid_identity(values["CONTROL_GENERATION"], 128)
            or "\0" in values["CONTROL_GENERATION"]):
        _fatal()
    for name in ("MAX_SUBJECTS", "MAX_SNAPSHOT_BYTES", "RETAINED_PAYLOAD_BUDGET"):
        text = values[name]
        if (not text.isascii() or not text.isdecimal() or len(text) > 19
                or not 0 < int(text) <= sys.maxsize or str(int(text)) != text):
            _fatal()
        values[name] = int(text)
    return values


def load(_loader: Any) -> None:
    global _owner, _server
    if _owner is not None or _server is not None:
        _fatal()
    owner = server = None
    try:
        values = _configuration(os.environ)
        owner = SubjectRevisionOwner(
            values["CONTROL_GENERATION"], max_subjects=values["MAX_SUBJECTS"],
            max_snapshot_bytes=values["MAX_SNAPSHOT_BYTES"],
            retained_payload_budget=values["RETAINED_PAYLOAD_BUDGET"],
        )
        server = Server(owner, values["SOCKET"], values["TOKEN"],
                        request_timeout=1, max_concurrent_requests=4)
        server.start()
    except Exception:  # noqa: BLE001 - never expose a token or handoff in errors
        if server is not None:
            with suppress(Exception):
                server.close()
        elif owner is not None:
            with suppress(Exception):
                owner.close()
        _fatal()
    _owner, _server = owner, server


def done() -> None:
    global _owner, _server
    if _server is not None:
        try:
            _server.close()
        except Exception:  # noqa: BLE001 - cleanup failure remains fatal and retryable
            _fatal()
        _owner = _server = None
