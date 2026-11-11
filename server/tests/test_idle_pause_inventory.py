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

from datetime import datetime, timezone

from opensandbox_server.extensions.keys import IDLE_POLICY_ANNOTATION_KEY
from opensandbox_server.integrations.idle_pause.inventory import (
    FastSandboxInventory,
    _candidate_from_cr,
)
from opensandbox_server.integrations.idle_pause.policy import (
    ActivityProbe,
    idle_policy_to_annotation,
    validate_idle_policy,
)


def _cr(
    *,
    name: str = "fsb-1",
    state: str = "Ready",
    ready: bool = True,
    expire_time: str | None = "2099-01-01T00:00:00Z",
    annotations: dict | None = None,
    deletion_timestamp: str | None = None,
) -> dict:
    metadata: dict = {
        "name": name,
        "uid": f"uid-{name}",
        "generation": 3,
        "creationTimestamp": "2026-01-01T00:00:00Z",
        "annotations": annotations or {},
    }
    if deletion_timestamp:
        metadata["deletionTimestamp"] = deletion_timestamp
    return {
        "metadata": metadata,
        "spec": {
            "image": "example:latest",
            "poolRef": "default-pool",
            "command": [],
            "args": [],
            "expireTime": expire_time,
        },
        "status": {
            "observedGeneration": 3,
            "runtime": {"state": state},
            "dataPlane": {"state": "Ready" if ready else "Pending"},
            "conditions": (
                [{"type": "Ready", "status": "True" if ready else "False",
                  "observedGeneration": 3}]
                if ready
                else []
            ),
        },
    }


def test_candidate_maps_state_policy_and_expiry():
    policy = validate_idle_policy(idle_timeout_seconds=300, on_idle="pause")
    cr = _cr(annotations={IDLE_POLICY_ANNOTATION_KEY: idle_policy_to_annotation(policy)})
    candidate = _candidate_from_cr(cr, "ns1")
    assert candidate is not None
    assert candidate.sandbox_id == "fsb-1"
    assert candidate.namespace == "ns1"
    assert candidate.uid == "uid-fsb-1"
    assert candidate.generation == 3
    assert candidate.state == "Running"
    assert candidate.policy == policy
    assert candidate.expires_at == datetime(2099, 1, 1, tzinfo=timezone.utc)


def test_candidate_without_policy_is_none_policy():
    candidate = _candidate_from_cr(_cr(), "ns1")
    assert candidate is not None
    assert candidate.policy is None


def test_candidate_decodes_probe_policy():
    policy = validate_idle_policy(
        idle_timeout_seconds=600,
        on_idle="pause",
        active_probe=ActivityProbe(port=9090, path="/busy", timeout_seconds=5),
    )
    candidate = _candidate_from_cr(_cr(annotations={IDLE_POLICY_ANNOTATION_KEY: idle_policy_to_annotation(policy)}), "ns1")
    assert candidate is not None and candidate.policy == policy


def test_candidate_maps_pause_and_deletion_states():
    paused = _candidate_from_cr(_cr(state="Paused"), "ns1")
    assert paused is not None and paused.state == "Paused"
    deleting = _candidate_from_cr(
        _cr(deletion_timestamp="2026-01-02T00:00:00Z"), "ns1"
    )
    assert deleting is not None and deleting.state == "Stopping"


def test_candidate_unparsable_expiry_drops_candidate():
    # The expiry gate cannot be verified, so the candidate is skipped.
    assert _candidate_from_cr(_cr(expire_time="not-a-time"), "ns1") is None


def test_inventory_survives_reader_failure():
    class ExplodingReader:
        def list(self, namespace: str) -> list[dict]:
            raise RuntimeError("apiserver down")

    assert FastSandboxInventory(ExplodingReader()).snapshot("ns1") == []  # type: ignore[arg-type]
