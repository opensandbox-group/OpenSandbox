# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Installation-only real Unix IPC child; no live TLS or transport admission.

The private stdin channel supplies the session and test controls. Only sanitized
metadata and pure classification results leave on stdout. Fault injection changes
network replies after the real owner has linearized the operation.
"""

import hashlib
import json
from pathlib import Path
import sys
import threading

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "mitmscripts"))
from subject_revision import SubjectRevisionOwner, SubjectError, Target
import subject_revision_ipc as ipc
from revision_ipc import _to_wire
from tls_decision import classify, compile_view


def emit(value):
    print(json.dumps(value), flush=True)


def main():
    config = json.loads(sys.stdin.readline())
    owner = SubjectRevisionOwner(
        config["control"], max_subjects=2, max_snapshot_bytes=65536,
        retained_payload_budget=262144,
    )
    lock = threading.Lock()
    state = {"drop": [], "dropped": 0, "abort_calls": 0, "last_abort": None}
    original_reply = ipc._Handler._reply

    def reply(handler, status, value):
        operation = handler.path.rsplit("/", 1)[-1]
        with lock:
            if operation == "abort" and status == 200:
                state["abort_calls"] += 1
                state["last_abort"] = value["revision"]
            if state["drop"] and state["drop"][0] == operation and status == 200:
                state["drop"].pop(0)
                state["dropped"] += 1
                handler.close_connection = True
                return
        original_reply(handler, status, value)

    ipc._Handler._reply = reply
    try:
        server = ipc.Server(
            owner, config["socket"], config["token"], request_timeout=1,
            max_concurrent_requests=4,
        )
    except ipc.ServerError:
        emit({"error": "subject revision IPC socket setup failed"})
        return
    server.start()
    emit({"ready": True})
    try:
        for line in sys.stdin:
            command = json.loads(line)
            if command["command"] == "fault":
                operations = command.get("operations", [command.get("operation")])
                if (type(operations) is not list or not 1 <= len(operations) <= 4
                        or any(op not in ("register", "unload", "prepare", "commit", "abort")
                               for op in operations)):
                    raise ValueError("invalid test fault")
                with lock:
                    state["drop"] = list(operations)
                emit({"ready": True})
            elif command["command"] == "inspect":
                target = Target(command["subjectKey"], command["subjectGeneration"])
                identity = (config["control"], target.subject_generation)
                try:
                    snapshot = owner.acquire(target)
                except SubjectError as exc:
                    if exc.outcome != "subject_unknown":
                        emit({"error": exc.outcome})
                        continue
                    snapshot, identity = None, None
                view = compile_view(snapshot) if snapshot else None
                decision = classify(
                    identity=identity, sni=command.get("sni"),
                    ech_hidden=command.get("ech", False),
                    static_passthrough=(), view=view,
                )
                with lock:
                    dropped = state["dropped"]
                    abort_calls, last_abort = state["abort_calls"], state["last_abort"]
                emit({
                    "revision": _to_wire(snapshot.revision) if snapshot else None,
                    "digest": hashlib.sha256(snapshot.payload).hexdigest() if snapshot else "",
                    "action": decision.action, "reason": decision.reason,
                    "dropped": dropped, "abortCalls": abort_calls, "lastAbort": last_abort,
                })
            else:
                raise ValueError("invalid test command")
    finally:
        server.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        emit({"error": "subject revision child failed"})
        sys.exit(1)
