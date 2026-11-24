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

"""Integration fixture for the production addon, without mitmdump/TLS/nft."""

import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "mitmscripts"))
import subject_runtime
from subject_revision import Target


def main():
    # Model the launcher's internal environment handoff; do not send secrets back.
    cfg = json.loads(sys.stdin.readline())
    prefix = "OPENSANDBOX_EGRESS_SUBJECT_REVISION_"
    for name, value in cfg.items():
        os.environ[prefix + name] = str(value)
    subject_runtime.load(None)
    print('{"ready":true}', flush=True)
    try:
        for line in sys.stdin:
            target = Target.from_wire(json.loads(line))
            snapshot = subject_runtime._owner.acquire(target)
            print(json.dumps({"known": True, "installed": snapshot is not None}), flush=True)
    finally:
        subject_runtime.done()


if __name__ == "__main__":
    main()
