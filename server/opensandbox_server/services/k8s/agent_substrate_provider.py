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

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass
from datetime import datetime, timezone
from threading import RLock, Timer
from typing import Any, Dict, List, Optional

import grpc

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    Endpoint,
    ImageSpec,
    NetworkPolicy,
    PlatformSpec,
    Volume,
)
from opensandbox_server.config import (
    AppConfig,
    AgentSubstrateTemplateConfig,
    EGRESS_MODE_DNS,
)
from opensandbox_server.services.k8s.agent_substrate_client import AgentSubstrateClient
from opensandbox_server.services.k8s.agent_substrate_proto import ateapi_pb2
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.workload_provider import WorkloadProvider
from opensandbox_server.well_known_extensions import AGENT_SUBSTRATE_TEMPLATE_KEY

_INVALID_ACTOR_CHARS = re.compile(r"[^a-z0-9-]+")


@dataclass
class _ActorRecord:
    sandbox_id: str
    actor_name: str
    atespace: str
    template: AgentSubstrateTemplateConfig
    labels: Dict[str, str]
    annotations: Dict[str, str]
    created_at: datetime
    expires_at: Optional[datetime]
    actor: Any


class AgentSubstrateProvider(WorkloadProvider):
    """POC provider that maps OpenSandbox sandboxes to Substrate Actors."""

    def __init__(
        self,
        k8s_client: K8sClient,
        app_config: Optional[AppConfig] = None,
        client: Optional[AgentSubstrateClient] = None,
    ):
        if app_config is None or app_config.agent_substrate is None:
            raise ValueError("Agent Substrate configuration is required")

        self.k8s_client = k8s_client
        self.config = app_config.agent_substrate
        self.client = client or AgentSubstrateClient(self.config)
        self._templates = {template.key: template for template in self.config.templates}
        self._records: Dict[str, _ActorRecord] = {}
        self._expiration_timers: Dict[str, Timer] = {}
        self._lock = RLock()

    def uses_kubernetes_managed_volumes(self) -> bool:
        return False

    def validate_create_request(self, request: CreateSandboxRequest) -> None:
        template_key = (request.extensions or {}).get(
            AGENT_SUBSTRATE_TEMPLATE_KEY, ""
        ).strip()
        if not template_key:
            raise ValueError(
                f'extensions["{AGENT_SUBSTRATE_TEMPLATE_KEY}"] is required for '
                "the agent-substrate provider."
            )
        if template_key not in self._templates:
            raise ValueError(
                f"Unknown Agent Substrate template key '{template_key}'."
            )

    @staticmethod
    def _actor_name(sandbox_id: str) -> str:
        normalized = _INVALID_ACTOR_CHARS.sub("-", sandbox_id.lower()).strip("-")
        candidate = f"osb-{normalized}" if normalized else "osb"
        if candidate == f"osb-{sandbox_id}" and len(candidate) <= 63:
            return candidate
        digest = hashlib.sha256(sandbox_id.encode("utf-8")).hexdigest()[:10]
        base = candidate[: 63 - len(digest) - 1].rstrip("-") or "osb"
        return f"{base}-{digest}"

    def create_workload(
        self,
        sandbox_id: str,
        namespace: str,
        image_spec: ImageSpec,
        entrypoint: List[str],
        env: Dict[str, str],
        resource_limits: Dict[str, str],
        labels: Dict[str, str],
        expires_at: Optional[datetime],
        execd_image: str,
        extensions: Optional[Dict[str, str]] = None,
        network_policy: Optional[NetworkPolicy] = None,
        egress_image: Optional[str] = None,
        volumes: Optional[List[Volume]] = None,
        platform: Optional[PlatformSpec] = None,
        annotations: Optional[Dict[str, str]] = None,
        egress_auth_token: Optional[str] = None,
        egress_mode: str = EGRESS_MODE_DNS,
        credential_proxy_enabled: bool = False,
        resource_requests: Optional[Dict[str, str]] = None,
        egress_env: Optional[Dict[str, Optional[str]]] = None,
    ) -> Dict[str, Any]:
        template_key = (extensions or {}).get(AGENT_SUBSTRATE_TEMPLATE_KEY, "").strip()
        template = self._templates.get(template_key)
        if template is None:
            raise ValueError(f"Unknown Agent Substrate template key '{template_key}'.")

        actor_name = self._actor_name(sandbox_id)
        record = _ActorRecord(
            sandbox_id=sandbox_id,
            actor_name=actor_name,
            atespace=self.config.default_atespace,
            template=template,
            labels=dict(labels),
            annotations=dict(annotations or {}),
            created_at=datetime.now(timezone.utc),
            expires_at=expires_at,
            actor=None,
        )
        with self._lock:
            if sandbox_id in self._records:
                raise ValueError(f"Sandbox {sandbox_id} already exists")
            self._records[sandbox_id] = record

        actor_created = False
        try:
            self.client.create_actor(
                atespace=record.atespace,
                actor_name=actor_name,
                template_namespace=template.namespace,
                template_name=template.name,
            )
            actor_created = True
            record.actor = self.client.resume_actor(record.atespace, actor_name)
        except Exception:
            with self._lock:
                self._records.pop(sandbox_id, None)
            if actor_created:
                try:
                    self.client.suspend_actor(record.atespace, actor_name)
                except Exception:
                    pass
                try:
                    self.client.delete_actor(record.atespace, actor_name)
                except Exception:
                    pass
            raise

        self._schedule_expiration(record)

        return {
            "name": actor_name,
            "uid": record.actor.metadata.uid,
            "apiVersion": "ateapi/v1",
            "kind": "Actor",
        }

    def _record(self, sandbox_id: str) -> Optional[_ActorRecord]:
        with self._lock:
            return self._records.get(sandbox_id)

    def _schedule_expiration(self, record: _ActorRecord) -> None:
        with self._lock:
            previous = self._expiration_timers.pop(record.sandbox_id, None)
            if previous is not None:
                previous.cancel()
            if record.expires_at is None:
                return

            delay = max(
                0.0,
                (record.expires_at - datetime.now(timezone.utc)).total_seconds(),
            )
            timer = Timer(delay, self._expire_sandbox, args=(record.sandbox_id,))
            timer.daemon = True
            self._expiration_timers[record.sandbox_id] = timer
            timer.start()

    def _expire_sandbox(self, sandbox_id: str) -> None:
        try:
            self.delete_workload(sandbox_id, namespace="")
        except Exception:
            return

    def _workload(self, record: _ActorRecord, actor: Any) -> Dict[str, Any]:
        record.actor = actor
        create_time = record.created_at
        if actor.metadata.HasField("create_time"):
            create_time = actor.metadata.create_time.ToDatetime(tzinfo=timezone.utc)
        return {
            "metadata": {
                "name": record.actor_name,
                "uid": actor.metadata.uid,
                "labels": dict(record.labels),
                "annotations": dict(record.annotations),
                "creationTimestamp": create_time,
            },
            "spec": {},
            "agentSubstrate": {
                "actor": actor,
                "record": record,
            },
        }

    def get_workload(self, sandbox_id: str, namespace: str) -> Optional[Dict[str, Any]]:
        record = self._record(sandbox_id)
        if record is None:
            return None
        try:
            actor = self.client.get_actor(record.atespace, record.actor_name)
        except grpc.RpcError as exc:
            if exc.code() == grpc.StatusCode.NOT_FOUND:
                return None
            raise
        return self._workload(record, actor)

    def delete_workload(self, sandbox_id: str, namespace: str) -> None:
        record = self._record(sandbox_id)
        if record is None:
            raise ValueError(f"Actor for sandbox {sandbox_id} not found")

        actor = self.client.get_actor(record.atespace, record.actor_name)
        if actor.status == ateapi_pb2.Actor.STATUS_PAUSED:
            actor = self.client.resume_actor(record.atespace, record.actor_name)
        if actor.status != ateapi_pb2.Actor.STATUS_SUSPENDED:
            self.client.suspend_actor(record.atespace, record.actor_name)
        self.client.delete_actor(record.atespace, record.actor_name)
        with self._lock:
            self._records.pop(sandbox_id, None)
            timer = self._expiration_timers.pop(sandbox_id, None)
            if timer is not None:
                timer.cancel()

    def list_workloads(self, namespace: str, label_selector: str) -> List[Dict[str, Any]]:
        actors = {
            actor.metadata.name: actor
            for actor in self.client.list_actors(self.config.default_atespace)
        }
        with self._lock:
            records = list(self._records.values())
        return [
            self._workload(record, actors[record.actor_name])
            for record in records
            if record.actor_name in actors
        ]

    def update_expiration(
        self,
        sandbox_id: str,
        namespace: str,
        expires_at: datetime,
    ) -> None:
        record = self._record(sandbox_id)
        if record is None:
            raise ValueError(f"Actor for sandbox {sandbox_id} not found")
        record.expires_at = expires_at
        self._schedule_expiration(record)

    def get_expiration(self, workload: Dict[str, Any]) -> Optional[datetime]:
        return workload["agentSubstrate"]["record"].expires_at

    def get_status(self, workload: Dict[str, Any]) -> Dict[str, Any]:
        actor = workload["agentSubstrate"]["actor"]
        state_map = {
            ateapi_pb2.Actor.STATUS_RESUMING: "Pending",
            ateapi_pb2.Actor.STATUS_RUNNING: "Running",
            ateapi_pb2.Actor.STATUS_SUSPENDING: "Pausing",
            ateapi_pb2.Actor.STATUS_SUSPENDED: "Paused",
            ateapi_pb2.Actor.STATUS_PAUSING: "Pausing",
            ateapi_pb2.Actor.STATUS_PAUSED: "Paused",
            ateapi_pb2.Actor.STATUS_CRASHED: "Failed",
        }
        state = state_map.get(actor.status, "Pending")
        status_name = ateapi_pb2.Actor.Status.Name(actor.status)
        last_transition_at = workload["metadata"]["creationTimestamp"]
        if actor.metadata.HasField("update_time"):
            last_transition_at = actor.metadata.update_time.ToDatetime(tzinfo=timezone.utc)
        return {
            "state": state,
            "reason": f"AGENT_SUBSTRATE_{status_name}",
            "message": f"Actor is {status_name.removeprefix('STATUS_').lower()}",
            "last_transition_at": last_transition_at,
        }

    def get_endpoint_info(
        self,
        workload: Dict[str, Any],
        port: int,
        sandbox_id: str,
    ) -> Optional[Endpoint]:
        record = workload["agentSubstrate"]["record"]
        if port != record.template.logical_port:
            return None
        host = f"{record.actor_name}.{record.atespace}.actors.resources.substrate.ate.dev"
        return Endpoint(
            endpoint=self.config.router_endpoint,
            headers={"Host": host},
        )

    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        record = self._record(sandbox_id)
        if record is None:
            raise ValueError(f"Actor for sandbox {sandbox_id} not found")
        record.actor = self.client.suspend_actor(record.atespace, record.actor_name)

    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        record = self._record(sandbox_id)
        if record is None:
            raise ValueError(f"Actor for sandbox {sandbox_id} not found")
        record.actor = self.client.resume_actor(record.atespace, record.actor_name)

    def patch_labels(
        self,
        name: str,
        namespace: str,
        labels: Dict[str, Optional[str]],
    ) -> Dict[str, Any]:
        with self._lock:
            record = next(
                (item for item in self._records.values() if item.actor_name == name),
                None,
            )
        if record is None:
            raise ValueError(f"Actor {name} not found")
        for key, value in labels.items():
            if value is None:
                record.labels.pop(key, None)
            else:
                record.labels[key] = value
        actor = self.client.get_actor(record.atespace, record.actor_name)
        return self._workload(record, actor)