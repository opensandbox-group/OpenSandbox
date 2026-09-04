# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""Map FastPath runtime observations to OpenSandbox lifecycle status.

fast-sandbox splits RuntimeReady (runtime up) from DataPlaneReady (routes and
Infra Components published). OpenSandbox reports Running only when both are
Ready, matching the "endpoint usable" expectation.

On expiry the reconciler keeps the Sandbox CRD with runtimeState=Stopped; that
retained object maps to Terminated.
"""

from __future__ import annotations

from opensandbox_server.services.fleets.generated import fastpath_pb2 as pb2


def map_state(info: pb2.SandboxInfo) -> str:
    """Map a fast-sandbox SandboxInfo to the OpenSandbox lifecycle state."""
    if (
        info.runtime.state == pb2.RUNTIME_STATE_STOPPING
        or info.data_plane.state == pb2.DATA_PLANE_STATE_DRAINING
    ):
        return "Stopping"
    if info.runtime.state == pb2.RUNTIME_STATE_STOPPED:
        return "Terminated"
    if info.runtime.state in (
        pb2.RUNTIME_STATE_FAILED,
        pb2.RUNTIME_STATE_UNAVAILABLE,
    ) or info.data_plane.state in (
        pb2.DATA_PLANE_STATE_FAILED,
        pb2.DATA_PLANE_STATE_UNAVAILABLE,
    ):
        return "Failed"
    if any(
        component.state == pb2.INFRA_COMPONENT_STATE_FAILED for component in info.infra_components
    ):
        return "Failed"
    if any(binding.state == pb2.ACTION_STATE_FAILED for binding in info.action_bindings):
        return "Failed"
    if info.ready:
        return "Running"
    return "Pending"


def map_reason(info: pb2.SandboxInfo) -> str | None:
    """Best-effort machine-readable reason for the mapped state.

    FastPath v2 SandboxInfo does not carry Conditions, so an Expired reason
    cannot be confirmed for a retained Stopped object; the reason is left
    unset rather than inventing a termination cause. Only states that are
    self-describing (Failed) report a reason.
    """
    if map_state(info) != "Failed":
        return None
    if info.runtime.state == pb2.RUNTIME_STATE_UNAVAILABLE:
        return "RuntimeUnavailable"
    if info.data_plane.state == pb2.DATA_PLANE_STATE_UNAVAILABLE:
        return "DataPlaneUnavailable"
    return "Failed"


__all__ = ["map_reason", "map_state"]
