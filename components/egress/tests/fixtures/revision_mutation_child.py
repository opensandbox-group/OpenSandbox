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

"""Real IPC child for Go owner integration tests, not a mitmdump/TLS harness.

Session credentials arrive only on stdin. stdout contains fixed diagnostics,
revision metadata and counters; neither payloads nor tokens are returned.
Faults change only replies after the real handler has performed its operation.
"""

import hashlib
import json
from pathlib import Path
import sys
import threading

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "mitmscripts"))
import revision_ipc as ipc
from revision_publication import InstallationReceiver


def emit(value):
    print(json.dumps(value), flush=True)


def main():
    config = json.loads(sys.stdin.readline())
    if config.get("LiveAdmission"):
        from revision_publication import LiveReceiver

        backend = LiveReceiver(
            config["ControlGeneration"], config["SubjectGeneration"],
            max_snapshot_bytes=config["MaxSnapshotBytes"],
            capacity=config["TLSCapacity"],
            request_capacity=config["RequestCapacity"],
            drain_timeout_seconds=config["DrainTimeoutSeconds"],
        )
    else:
        backend = InstallationReceiver(
            config["ControlGeneration"], config["SubjectGeneration"],
            max_snapshot_bytes=config["MaxSnapshotBytes"],
        )
    lock = threading.Lock()
    state = {"fault": "none", "dropped": 0, "prepare": 0, "commit": 0, "abort": 0, "readback": 0}
    readback_entered = threading.Event()
    release_readback = threading.Event()
    original_reply = ipc._Handler._reply

    def reply(handler, status, value):
        operation = handler.path.rsplit("/", 1)[-1]
        hold = False
        with lock:
            if status == 200:
                if operation in ("prepare", "commit", "abort"):
                    state[operation] += 1
                elif operation == "active":
                    state["readback"] += 1
                if operation == "prepare" and state["fault"] == "drop-prepare":
                    state["dropped"] += 1
                    state["fault"] = "none"
                    handler.close_connection = True
                    return
                if operation == "commit" and state["fault"] != "none":
                    state["dropped"] += 1
                    handler.close_connection = True
                    return
                if operation == "active" and state["dropped"] and state["fault"] in ("drop-commit", "unresolved"):
                    readback_entered.set()
                    hold = True
                    if state["fault"] == "unresolved":
                        status, value = 503, {"error": "unavailable"}
        if hold and not release_readback.wait(5):
            handler.close_connection = True
            return
        original_reply(handler, status, value)

    ipc._Handler._reply = reply
    try:
        server = ipc.Server(
            backend, config["SocketPath"], config["SessionToken"],
            max_snapshot_bytes=config["MaxSnapshotBytes"], request_timeout=2,
        )
    except ipc.ServerError:
        emit({"error": "revision integration IPC socket setup failed"})
        return
    server.start()
    emit({"ready": True})
    try:
        for line in sys.stdin:
            command = json.loads(line)
            if command["command"] == "fault":
                with lock:
                    state["fault"] = command["mode"]
                emit({"ready": True})
            elif command["command"] == "await-readback":
                emit({"ready": readback_entered.wait(3)})
            elif command["command"] == "release":
                release_readback.set()
                emit({"ready": True})
            elif command["command"] == "inspect":
                snapshot = backend.acquire()
                view = backend._publisher.registry._view
                with lock:
                    counts = dict(state)
                emit({
                    "active": ipc._to_wire(backend.readback()),
                    "view": ipc._to_wire(view.revision) if view else None,
                    "digest": hashlib.sha256(snapshot.payload).hexdigest() if snapshot else "",
                    "admission_disabled": backend._publisher.registry._admission_disabled,
                    "counts": counts,
                })
            else:
                raise ValueError("invalid command")
    finally:
        server.close()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        emit({"error": "revision integration child failed"})
        sys.exit(1)
