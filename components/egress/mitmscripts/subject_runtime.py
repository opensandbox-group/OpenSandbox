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

"""Launcher-selected subject receiver and optional B3b-1 handshake experiment."""

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
_ADMISSION_FIELDS = {"TLS_ADMISSION", "TLS_CAPACITY", "PROCESS_TLS_CAPACITY",
                     "DRAIN_TIMEOUT_SECONDS"}
_ERROR = "credential proxy: invalid revision runtime configuration"
_owner: SubjectRevisionOwner | None = None
_server: Server | None = None
_admission_enabled = False


def _fatal() -> NoReturn:
    # Script watcher exceptions can otherwise leave a listener without its addon.
    raise SystemExit(_ERROR) from None


def _configuration(environ: Mapping[str, str]) -> dict[str, Any]:
    values = {name[len(_PREFIX):]: value for name, value in environ.items()
              if name.startswith(_PREFIX)}
    fields = _FIELDS | _ADMISSION_FIELDS if "TLS_ADMISSION" in values else _FIELDS
    if set(values) != fields or values["MODE"] != "subject-v2":
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
    if "TLS_ADMISSION" in values:
        if values["TLS_ADMISSION"] != "true":
            _fatal()
        for name in ("TLS_CAPACITY", "PROCESS_TLS_CAPACITY"):
            text = values[name]
            if (not text.isascii() or not text.isdecimal() or len(text) > 7
                    or not 1 <= int(text) <= 1 << 20 or str(int(text)) != text):
                _fatal()
            values[name] = int(text)
        text = values["DRAIN_TIMEOUT_SECONDS"]
        if (not text.isascii() or not text.isdecimal() or len(text) > 3
                or not 1 <= int(text) <= 300 or str(int(text)) != text):
            _fatal()
        values["DRAIN_TIMEOUT_SECONDS"] = int(text)
    return values


def load(_loader: Any) -> None:
    global _owner, _server, _admission_enabled
    if _owner is not None or _server is not None:
        _fatal()
    owner = server = runtime = None
    try:
        values = _configuration(os.environ)
        admission = {}
        if "TLS_ADMISSION" in values:
            from credential_bound import validate_live_options
            from static_passthrough import ENV, parse_handoff

            validate_live_options()
            if os.environ.get("OPENSANDBOX_EGRESS_UPSTREAM_PROXY", "").strip():
                _fatal()
            admission = dict(tls_capacity=values["TLS_CAPACITY"],
                             process_tls_capacity=values["PROCESS_TLS_CAPACITY"],
                             static_passthrough=parse_handoff(os.environ.get(ENV)),
                             drain_timeout_seconds=values["DRAIN_TIMEOUT_SECONDS"])
        owner = SubjectRevisionOwner(
            values["CONTROL_GENERATION"], max_subjects=values["MAX_SUBJECTS"],
            max_snapshot_bytes=values["MAX_SNAPSHOT_BYTES"],
            retained_payload_budget=values["RETAINED_PAYLOAD_BUDGET"],
            **admission,
        )
        server = Server(owner, values["SOCKET"], values["TOKEN"],
                        request_timeout=1, max_concurrent_requests=4)
        server.start()
        if admission:
            import subject_admission
            runtime = subject_admission.SubjectAdmissionRuntime(owner)
            # One process-level drain loop, only when admission is enabled.
            runtime.start()
    except Exception:  # noqa: BLE001 - never expose a token or handoff in errors
        if runtime is not None:
            with suppress(Exception):
                runtime.close()
        if server is not None:
            with suppress(Exception):
                server.close()
        elif owner is not None:
            with suppress(Exception):
                owner.close()
        _fatal()
    _owner, _server = owner, server
    _admission_enabled = bool(admission)
    if _admission_enabled:
        import subject_admission
        subject_admission._runtime = runtime


def done() -> None:
    global _owner, _server, _admission_enabled
    if _server is not None:
        if _admission_enabled:
            import subject_admission
            runtime = subject_admission._runtime
            if runtime is not None:
                # Stop sweeping before the owner deactivates its registries.
                runtime.close()
        try:
            _server.close()
        except Exception:  # noqa: BLE001 - cleanup failure remains fatal and retryable
            _fatal()
        _owner = _server = None
        if _admission_enabled:
            import subject_admission
            subject_admission._runtime = None
        _admission_enabled = False
