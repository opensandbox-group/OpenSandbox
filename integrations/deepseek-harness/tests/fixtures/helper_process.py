# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Test-only subprocess synchronization at real helper filesystem boundaries."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import time

helper_path, request, response, boundary, ready_path, gate_path = sys.argv[1:]
spec = importlib.util.spec_from_file_location("fs_helper", helper_path)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
ready, gate = Path(ready_path), Path(gate_path)
original_fsync = helper.os.fsync
original_flock = helper.fcntl.flock


def fsync(fd):
    original_fsync(fd)
    path = os.readlink(f"/proc/self/fd/{fd}")
    if boundary == "stage-synced" and path.endswith(".tmpdir/payload"):
        ready.write_text(json.dumps({"stagingDir": str(Path(path).parent), "tempPath": path}))
        while not gate.exists() and not helper._cancelled:
            time.sleep(0.01)


def flock(fd, operation):
    try:
        return original_flock(fd, operation)
    except BlockingIOError:
        if boundary == "lock-wait":
            ready.write_text(json.dumps({"waitingForLock": True}))
        raise


helper.fcntl.flock = flock
helper.os.fsync = fsync
sys.exit(helper.main([request, response]))
