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

from pathlib import Path
from typing import Any, Optional

import grpc

from opensandbox_server.config import AgentSubstrateRuntimeConfig
from opensandbox_server.services.k8s.agent_substrate_proto import (
    ateapi_pb2,
    ateapi_pb2_grpc,
)


class _FileTokenAuth(grpc.AuthMetadataPlugin):
    def __init__(self, token_file: str):
        self._token_file = token_file

    def __call__(self, context: Any, callback: Any) -> None:
        try:
            token = Path(self._token_file).read_text(encoding="utf-8").strip()
            if not token:
                raise ValueError(f"Agent Substrate token file is empty: {self._token_file}")
            callback((("authorization", f"Bearer {token}"),), None)
        except Exception as exc:
            callback((), exc)


class AgentSubstrateClient:
    """Synchronous client for the pinned Agent Substrate Control API."""

    def __init__(
        self,
        config: AgentSubstrateRuntimeConfig,
        stub: Optional[Any] = None,
    ):
        self._timeout = config.rpc_timeout_seconds
        self._channel: Optional[grpc.Channel] = None

        if stub is not None:
            self._stub = stub
            return

        root_certificates = Path(config.ca_file).read_bytes()
        channel_credentials: grpc.ChannelCredentials = grpc.ssl_channel_credentials(
            root_certificates=root_certificates
        )
        if config.token_file:
            call_credentials = grpc.metadata_call_credentials(
                _FileTokenAuth(config.token_file)
            )
            channel_credentials = grpc.composite_channel_credentials(
                channel_credentials,
                call_credentials,
            )

        self._channel = grpc.secure_channel(
            config.api_endpoint,
            channel_credentials,
            options=(("grpc.ssl_target_name_override", config.server_name),),
        )
        self._stub = ateapi_pb2_grpc.ControlStub(self._channel)

    def close(self) -> None:
        if self._channel is not None:
            self._channel.close()

    @staticmethod
    def actor_ref(atespace: str, actor_name: str) -> Any:
        return ateapi_pb2.ObjectRef(atespace=atespace, name=actor_name)

    def create_actor(
        self,
        atespace: str,
        actor_name: str,
        template_namespace: str,
        template_name: str,
    ) -> Any:
        request = ateapi_pb2.CreateActorRequest(
            actor=ateapi_pb2.Actor(
                metadata=ateapi_pb2.ResourceMetadata(
                    atespace=atespace,
                    name=actor_name,
                ),
                actor_template_namespace=template_namespace,
                actor_template_name=template_name,
            )
        )
        return self._stub.CreateActor(request, timeout=self._timeout)

    def get_actor(self, atespace: str, actor_name: str) -> Any:
        request = ateapi_pb2.GetActorRequest(
            actor=self.actor_ref(atespace, actor_name)
        )
        return self._stub.GetActor(request, timeout=self._timeout)

    def list_actors(self, atespace: str) -> list[Any]:
        actors: list[Any] = []
        page_token = ""
        while True:
            response = self._stub.ListActors(
                ateapi_pb2.ListActorsRequest(
                    atespace=atespace,
                    page_size=1000,
                    page_token=page_token,
                ),
                timeout=self._timeout,
            )
            actors.extend(response.actors)
            page_token = response.next_page_token
            if not page_token:
                return actors

    def suspend_actor(self, atespace: str, actor_name: str) -> Any:
        response = self._stub.SuspendActor(
            ateapi_pb2.SuspendActorRequest(
                actor=self.actor_ref(atespace, actor_name)
            ),
            timeout=self._timeout,
        )
        return response.actor

    def resume_actor(self, atespace: str, actor_name: str) -> Any:
        response = self._stub.ResumeActor(
            ateapi_pb2.ResumeActorRequest(
                actor=self.actor_ref(atespace, actor_name)
            ),
            timeout=self._timeout,
        )
        return response.actor

    def delete_actor(self, atespace: str, actor_name: str) -> Any:
        return self._stub.DeleteActor(
            ateapi_pb2.DeleteActorRequest(
                actor=self.actor_ref(atespace, actor_name)
            ),
            timeout=self._timeout,
        )