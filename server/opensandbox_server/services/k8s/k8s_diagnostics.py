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

"""
Kubernetes diagnostics mixin for DevOps API.

Provides get_sandbox_logs, get_sandbox_inspect, and get_sandbox_events
by querying K8s Pod state and events. Mixed into KubernetesSandboxService.
"""

from __future__ import annotations

import re

from fastapi import HTTPException, status

from opensandbox_server.services.constants import (
    SANDBOX_ID_LABEL,
    SandboxErrorCodes,
)


def _parse_since(since: str) -> int:
    """Parse a human-readable duration string (e.g. '10m', '1h') into seconds."""
    m = re.fullmatch(r"(\d+)\s*([smhd])", since.strip().lower())
    if not m:
        return 600
    value, unit = int(m.group(1)), m.group(2)
    multipliers = {"s": 1, "m": 60, "h": 3600, "d": 86400}
    return value * multipliers[unit]


class K8sDiagnosticsMixin:
    """Mixin that implements diagnostics methods for the Kubernetes backend."""

    def _find_pod_for_sandbox(self, sandbox_id: str):
        """Find the Pod associated with a sandbox ID via label selector."""
        label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        try:
            pods = self.k8s_client.list_pods(
                namespace=self.namespace,
                label_selector=label_selector,
            )
        except Exception as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_API_ERROR,
                    "message": f"Failed to query pods for sandbox {sandbox_id}: {exc}",
                },
            ) from exc

        if not pods:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.K8S_SANDBOX_NOT_FOUND,
                    "message": f"No pod found for sandbox '{sandbox_id}'",
                },
            )
        return pods[0]

    def get_sandbox_logs(self, sandbox_id: str, tail: int = 100, since: str | None = None) -> str:
        pod = self._find_pod_for_sandbox(sandbox_id)
        pod_name = pod.metadata.name
        core_v1 = self.k8s_client.get_core_v1_api()

        kwargs: dict = {
            "name": pod_name,
            "namespace": self.namespace,
            "tail_lines": tail,
            "timestamps": True,
        }
        if since:
            kwargs["since_seconds"] = _parse_since(since)

        log_text = core_v1.read_namespaced_pod_log(**kwargs)
        return log_text or "(no logs)"

    def get_sandbox_inspect(self, sandbox_id: str) -> str:
        pod = self._find_pod_for_sandbox(sandbox_id)
        meta = pod.metadata
        spec = pod.spec
        pod_status = pod.status

        lines: list[str] = []
        lines.append(f"Pod Name:       {meta.name}")
        lines.append(f"Namespace:      {meta.namespace}")
        lines.append(f"Node:           {spec.node_name or 'N/A'}")
        lines.append(f"Phase:          {pod_status.phase if pod_status else 'Unknown'}")
        lines.append(f"Pod IP:         {pod_status.pod_ip if pod_status else 'N/A'}")
        lines.append(f"Host IP:        {pod_status.host_ip if pod_status else 'N/A'}")
        lines.append(f"Start Time:     {pod_status.start_time if pod_status else 'N/A'}")

        if spec.runtime_class_name:
            lines.append(f"Runtime Class:  {spec.runtime_class_name}")

        # Container statuses
        if pod_status and pod_status.container_statuses:
            lines.append("")
            lines.append("Containers:")
            for cs in pod_status.container_statuses:
                lines.append(f"  {cs.name}:")
                lines.append(f"    Ready:          {cs.ready}")
                lines.append(f"    Restart Count:  {cs.restart_count}")
                lines.append(f"    Image:          {cs.image}")
                if cs.state:
                    if cs.state.running:
                        lines.append(f"    State:          Running (since {cs.state.running.started_at})")
                    elif cs.state.waiting:
                        lines.append(f"    State:          Waiting ({cs.state.waiting.reason})")
                        if cs.state.waiting.message:
                            lines.append(f"    Message:        {cs.state.waiting.message}")
                    elif cs.state.terminated:
                        t = cs.state.terminated
                        lines.append(f"    State:          Terminated (exit={t.exit_code}, reason={t.reason})")
                        if t.message:
                            lines.append(f"    Message:        {t.message}")
                if cs.last_state and cs.last_state.terminated:
                    t = cs.last_state.terminated
                    lines.append(f"    Last State:     Terminated (exit={t.exit_code}, reason={t.reason})")

        # Init container statuses
        if pod_status and pod_status.init_container_statuses:
            lines.append("")
            lines.append("Init Containers:")
            for cs in pod_status.init_container_statuses:
                lines.append(f"  {cs.name}:")
                lines.append(f"    Ready:          {cs.ready}")
                if cs.state:
                    if cs.state.terminated:
                        t = cs.state.terminated
                        lines.append(f"    State:          Terminated (exit={t.exit_code}, reason={t.reason})")
                    elif cs.state.waiting:
                        lines.append(f"    State:          Waiting ({cs.state.waiting.reason})")

        # Conditions
        if pod_status and pod_status.conditions:
            lines.append("")
            lines.append("Conditions:")
            for cond in pod_status.conditions:
                lines.append(f"  {cond.type}: {cond.status} (reason={cond.reason or 'N/A'})")
                if cond.message:
                    lines.append(f"    Message: {cond.message}")

        # Labels
        if meta.labels:
            lines.append("")
            lines.append("Labels:")
            for k, v in sorted(meta.labels.items()):
                lines.append(f"  {k}={v}")

        # Resource requests/limits
        if spec.containers:
            lines.append("")
            lines.append("Resources:")
            for container in spec.containers:
                if container.resources:
                    lines.append(f"  {container.name}:")
                    if container.resources.requests:
                        lines.append(f"    Requests: {dict(container.resources.requests)}")
                    if container.resources.limits:
                        lines.append(f"    Limits:   {dict(container.resources.limits)}")

        return "\n".join(lines)

    def get_sandbox_events(self, sandbox_id: str, limit: int = 50) -> str:
        pod = self._find_pod_for_sandbox(sandbox_id)
        pod_name = pod.metadata.name
        core_v1 = self.k8s_client.get_core_v1_api()

        events_resp = core_v1.list_namespaced_event(
            namespace=self.namespace,
            field_selector=f"involvedObject.name={pod_name}",
            limit=limit,
        )

        if not events_resp.items:
            return "(no events)"

        lines: list[str] = []
        for ev in events_resp.items:
            ts = ev.last_timestamp or ev.event_time or ev.first_timestamp or "N/A"
            lines.append(
                f"[{ts}] {ev.type:8s} {ev.reason or 'N/A':20s} {ev.message or ''}"
            )
        return "\n".join(lines)
