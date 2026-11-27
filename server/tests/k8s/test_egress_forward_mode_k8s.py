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

"""OSEP-0030 egress forward mode on Kubernetes (non-pooled BatchSandbox + AgentSandbox)."""

from datetime import datetime, timezone
from unittest.mock import MagicMock, patch

import pytest
import yaml
from fastapi import HTTPException, status
from pydantic import SecretStr

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CredentialProxyConfig,
    ImageSpec,
    NetworkPolicy,
    PlatformSpec,
    ResourceLimits,
)
from opensandbox_server.config import (
    AgentSandboxRuntimeConfig,
    AppConfig,
    EGRESS_MODE_DNS_NFT,
    EGRESS_MODE_FORWARD,
    EgressConfig,
    EgressUpstreamProxyConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
    ServerConfig,
)
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RESOLVER_MOUNT_PATH,
    EGRESS_RESOLVER_VOLUME_NAME,
    EGRESS_RULES_ENV,
    EGRESS_UPSTREAM_EXTRA_CA_PATH,
    EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME,
    OPENSANDBOX_EGRESS_DNS_UPSTREAM,
    OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA,
    OPENSANDBOX_EGRESS_SANDBOX_ID,
    OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE,
    OPENSANDBOX_EGRESS_TENANT_NAME,
    OPENSANDBOX_EGRESS_UPSTREAM_PROXY,
    OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH,
)
from opensandbox_server.services.fast_sandbox import FastSandboxService
from opensandbox_server.services.k8s.agent_sandbox_provider import AgentSandboxProvider
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.egress_helper import (
    apply_egress_to_spec,
    build_egress_bootstrap_init_container,
    build_security_context_for_sandbox_container,
    validate_forward_mode_pod_spec,
)
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.k8s.workload_provider import EgressWorkloadSettings
from opensandbox_server.tenants.context import set_current_tenant
from opensandbox_server.tenants.models import TenantEntry

GATEWAY = EgressUpstreamProxyConfig(
    url="https://gateway.example.com:3000",
    authorization=SecretStr("Bearer gw-token"),
    ca_secret_name="gateway-ca",
)


def _forward_egress(**overrides) -> EgressConfig:
    fields = {
        "image": "opensandbox/egress:v1.2.0",
        "mode": EGRESS_MODE_FORWARD,
        "upstream_proxy": GATEWAY,
        "dns_upstreams": ["10.96.0.53:53"],
        "requests": {"cpu": "25m", "memory": "64Mi"},
        "limits": {"cpu": "250m", "memory": "256Mi"},
    }
    fields.update(overrides)
    return EgressConfig(**fields)


def _k8s_app_config(
    *,
    workload_provider: str = "batchsandbox",
    namespace: str = "opensandbox",
    template_file: str | None = None,
    egress: EgressConfig | None = None,
) -> AppConfig:
    k8s_kwargs: dict = {
        "namespace": namespace,
        "workload_provider": workload_provider,
    }
    if template_file and workload_provider == "batchsandbox":
        k8s_kwargs["batchsandbox_template_file"] = template_file
    extra: dict = {}
    if workload_provider == "agent-sandbox":
        extra["agent_sandbox"] = AgentSandboxRuntimeConfig(
            template_file=template_file,
        )
    return AppConfig(
        server=ServerConfig(),
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(**k8s_kwargs),
        egress=egress or _forward_egress(),
        **extra,
    )


def _forward_settings(**overrides) -> EgressWorkloadSettings:
    fields = {
        "network_policy": None,
        "image": "opensandbox/egress:v1.2.0",
        "mode": EGRESS_MODE_FORWARD,
        "auth_token": "egress-token",
        "credential_proxy_enabled": False,
        "env": {},
        "disable_ipv6": True,
        "resource_requests": {"cpu": "25m", "memory": "64Mi"},
        "resource_limits": {"cpu": "250m", "memory": "256Mi"},
        "upstream_proxy": GATEWAY,
        "dns_upstreams": ["10.96.0.53:53"],
        "sandbox_namespace": "team-a-ns",
        "tenant_name": None,
    }
    fields.update(overrides)
    return EgressWorkloadSettings(**fields)


def _create_request(**overrides) -> CreateSandboxRequest:
    fields = {
        "image": ImageSpec(uri="python:3.11"),
        "timeout": 120,
        "resourceLimits": ResourceLimits(root={"cpu": "500m", "memory": "512Mi"}),
        "env": {"APP": "1"},
        "metadata": {},
        "entrypoint": ["python"],
    }
    fields.update(overrides)
    return CreateSandboxRequest(**fields)


def _env_map(container: dict) -> dict[str, str]:
    return {item["name"]: item["value"] for item in (container.get("env") or [])}


# --- egress_helper ------------------------------------------------------------


class TestForwardModeEgressHelper:
    def test_apply_adds_bootstrap_init_and_sidecar_without_rules(self):
        containers: list[dict] = []
        volumes: list[dict] = []
        init_containers: list[dict] = [{"name": "execd-installer"}]

        apply_egress_to_spec(
            containers=containers,
            egress_settings=_forward_settings(tenant_name="acme"),
            sandbox_id="sbx-1",
            pod_volumes=volumes,
            init_containers=init_containers,
        )

        # Bootstrap default-deny fence runs in initContainers before workload containers.
        assert [c["name"] for c in init_containers] == ["execd-installer", "egress-init"]
        egress_init = init_containers[1]
        assert egress_init["image"] == "opensandbox/egress:v1.2.0"
        assert egress_init["securityContext"] == {"capabilities": {"add": ["NET_ADMIN"]}}
        assert egress_init["resources"] == {
            "requests": {"cpu": "25m", "memory": "64Mi"},
            "limits": {"cpu": "250m", "memory": "256Mi"},
        }
        script = egress_init["args"][0]
        assert f"cp /etc/hosts {EGRESS_RESOLVER_MOUNT_PATH}/hosts" in script
        assert f"cp /etc/resolv.conf {EGRESS_RESOLVER_MOUNT_PATH}/resolv.conf" in script
        assert "table inet opensandbox" in script
        assert "policy drop;" in script
        assert 'oifname "lo" accept' in script
        assert {
            "name": EGRESS_RESOLVER_VOLUME_NAME,
            "mountPath": EGRESS_RESOLVER_MOUNT_PATH,
        } in egress_init["volumeMounts"]

        assert len(containers) == 1
        sidecar = containers[0]
        env = _env_map(sidecar)
        assert EGRESS_RULES_ENV not in env
        assert env[EGRESS_MODE_ENV] == EGRESS_MODE_FORWARD
        assert env[OPENSANDBOX_EGRESS_UPSTREAM_PROXY] == "https://gateway.example.com:3000"
        assert env[OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH] == "Bearer gw-token"
        assert env[OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA] == EGRESS_UPSTREAM_EXTRA_CA_PATH
        assert env[OPENSANDBOX_EGRESS_DNS_UPSTREAM] == "10.96.0.53:53"
        assert env[OPENSANDBOX_EGRESS_SANDBOX_ID] == "sbx-1"
        assert env[OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE] == "team-a-ns"
        assert env[OPENSANDBOX_EGRESS_TENANT_NAME] == "acme"
        assert any(v["name"] == EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME for v in volumes)
        assert {"name": EGRESS_RESOLVER_VOLUME_NAME, "emptyDir": {}} in volumes
        assert {
            "name": EGRESS_RESOLVER_VOLUME_NAME,
            "mountPath": EGRESS_RESOLVER_MOUNT_PATH,
            "readOnly": True,
        } in sidecar["volumeMounts"]

    def test_apply_requires_init_containers_and_volumes_in_forward_mode(self):
        with pytest.raises(ValueError, match="init_containers"):
            apply_egress_to_spec(
                containers=[],
                egress_settings=_forward_settings(),
                sandbox_id="sbx-1",
                pod_volumes=[],
            )
        with pytest.raises(ValueError, match="pod_volumes"):
            apply_egress_to_spec(
                containers=[],
                egress_settings=_forward_settings(),
                sandbox_id="sbx-1",
                init_containers=[],
            )

    def test_non_forward_never_emits_namespace_or_tenant_or_bootstrap_init(self):
        containers: list[dict] = []
        init_containers: list[dict] = [{"name": "execd-installer"}]
        settings = _forward_settings(
            network_policy=NetworkPolicy(default_action="deny", egress=[]),
            mode=EGRESS_MODE_DNS_NFT,
            sandbox_namespace="team-a-ns",
            tenant_name="acme",
        )
        apply_egress_to_spec(
            containers=containers,
            egress_settings=settings,
            sandbox_id="sbx-1",
            pod_volumes=[],
            init_containers=init_containers,
        )
        assert len(init_containers) == 1
        env = _env_map(containers[0])
        assert EGRESS_RULES_ENV in env
        assert OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE not in env
        assert OPENSANDBOX_EGRESS_TENANT_NAME not in env
        assert OPENSANDBOX_EGRESS_DNS_UPSTREAM not in env

    def test_sandbox_security_context_drops_net_admin_and_net_raw(self):
        assert build_security_context_for_sandbox_container(True, drop_net_raw=True) == {
            "capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]}
        }
        assert build_security_context_for_sandbox_container(True, drop_net_raw=False) == {
            "capabilities": {"drop": ["NET_ADMIN"]}
        }

    def test_bootstrap_init_omits_resources_when_unconfigured(self):
        init_c = build_egress_bootstrap_init_container(
            _forward_settings(resource_requests=None, resource_limits=None)
        )
        assert "resources" not in init_c

    @pytest.mark.parametrize(
        "pod_spec, match",
        [
            (
                {
                    "shareProcessNamespace": True,
                    "containers": [
                        {"name": "sandbox", "securityContext": {"capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]}}}
                    ],
                },
                "shareProcessNamespace",
            ),
            (
                {
                    "hostPID": True,
                    "containers": [
                        {"name": "sandbox", "securityContext": {"capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]}}}
                    ],
                },
                "hostPID",
            ),
            (
                {
                    "hostNetwork": True,
                    "containers": [
                        {"name": "sandbox", "securityContext": {"capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]}}}
                    ],
                },
                "hostNetwork",
            ),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {
                                "privileged": True,
                                "capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]},
                            },
                        }
                    ]
                },
                "privileged",
            ),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {
                                "capabilities": {
                                    "add": ["CAP_NET_RAW"],
                                    "drop": ["NET_ADMIN", "NET_RAW"],
                                }
                            },
                        }
                    ]
                },
                "NET_RAW",
            ),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {"capabilities": {"drop": ["NET_ADMIN"]}},
                        }
                    ]
                },
                "must drop NET_ADMIN and NET_RAW",
            ),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {"capabilities": {"drop": ["NET_ADMIN", "NET_RAW"]}},
                            "volumeMounts": [
                                {"name": EGRESS_RESOLVER_VOLUME_NAME, "mountPath": "/tamper"}
                            ],
                        }
                    ]
                },
                EGRESS_RESOLVER_VOLUME_NAME,
            ),
        ],
    )
    def test_validate_forward_mode_pod_spec_rejects_violations(self, pod_spec, match):
        with pytest.raises(ValueError, match=match):
            validate_forward_mode_pod_spec(pod_spec)


# --- Providers: BatchSandbox & AgentSandbox -----------------------------------


class TestBatchSandboxForwardMode:
    def test_template_mode_attaches_bootstrap_fence_and_drops_caps(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client, _k8s_app_config())
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sbx-1", "uid": "uid-1"}
        }

        provider.create_workload(
            sandbox_id="sbx-1",
            namespace="team-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["python"],
            env={"APP": "1"},
            resource_limits={"cpu": "500m", "memory": "512Mi"},
            labels={"opensandbox.io/id": "sbx-1"},
            expires_at=datetime(2026, 12, 31, tzinfo=timezone.utc),
            execd_image="opensandbox/execd:test",
            egress_settings=_forward_settings(sandbox_namespace="team-ns"),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        assert [c["name"] for c in pod_spec["initContainers"]] == ["execd-installer", "egress-init"]
        assert [c["name"] for c in pod_spec["containers"]] == ["sandbox", "egress"]

        sandbox_c, egress_c = pod_spec["containers"]
        assert set(sandbox_c["securityContext"]["capabilities"]["drop"]) == {"NET_ADMIN", "NET_RAW"}
        assert not any(e["name"].startswith("OPENSANDBOX_EGRESS_") for e in sandbox_c.get("env", []))
        assert not any(
            m["name"] in (EGRESS_UPSTREAM_EXTRA_CA_VOLUME_NAME, EGRESS_RESOLVER_VOLUME_NAME)
            for m in sandbox_c.get("volumeMounts", [])
        )

        egress_env = _env_map(egress_c)
        assert egress_env[EGRESS_MODE_ENV] == EGRESS_MODE_FORWARD
        assert egress_env[OPENSANDBOX_EGRESS_SANDBOX_ID] == "sbx-1"
        assert egress_env[OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE] == "team-ns"
        assert OPENSANDBOX_EGRESS_TENANT_NAME not in egress_env

    @pytest.mark.parametrize(
        "template_pod_spec, expected_exc, match",
        [
            ({"shareProcessNamespace": True}, ValueError, "shareProcessNamespace"),
            ({"hostPID": True}, ValueError, "hostPID"),
            ({"hostNetwork": True}, ValueError, "hostNetwork"),
            ({"runtimeClassName": "gvisor"}, HTTPException, "gVisor"),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {"capabilities": {"add": ["NET_ADMIN"]}},
                        }
                    ]
                },
                ValueError,
                "NET_ADMIN",
            ),
            (
                {
                    "containers": [
                        {
                            "name": "sandbox",
                            "securityContext": {"capabilities": {"add": ["NET_RAW"]}},
                        }
                    ]
                },
                ValueError,
                "NET_RAW",
            ),
            (
                {
                    "containers": [
                        {"name": "sandbox", "securityContext": {"privileged": True}}
                    ]
                },
                ValueError,
                "privileged",
            ),
        ],
    )
    def test_rejects_unsafe_batchsandbox_pod_template(
        self, tmp_path, mock_k8s_client, template_pod_spec, expected_exc, match
    ):
        tpl = tmp_path / "template.yaml"
        tpl.write_text(
            yaml.safe_dump({"spec": {"template": {"spec": template_pod_spec}}})
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _k8s_app_config(template_file=str(tpl))
        )

        with pytest.raises(expected_exc) as exc:
            provider.create_workload(
                sandbox_id="sbx-1",
                namespace="team-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["python"],
                env={},
                resource_limits={},
                labels={},
                expires_at=None,
                execd_image="opensandbox/execd:test",
                egress_settings=_forward_settings(sandbox_namespace="team-ns"),
            )

        assert match in str(exc.value.detail if isinstance(exc.value, HTTPException) else exc.value)
        mock_k8s_client.create_custom_object.assert_not_called()

    @pytest.mark.parametrize(
        "template_spec_extra, match",
        [
            ({"shardPatches": [{"spec": {"hostNetwork": True}}]}, "shardPatches"),
            ({"shardPatches": [{}]}, "shardPatches"),
            ({"poolRef": "warm-pool"}, "poolRef"),
        ],
    )
    def test_rejects_unsafe_batchsandbox_spec_fields_in_forward_mode(
        self, tmp_path, mock_k8s_client, template_spec_extra, match
    ):
        tpl = tmp_path / "template.yaml"
        tpl.write_text(yaml.safe_dump({"spec": template_spec_extra}))
        provider = BatchSandboxProvider(
            mock_k8s_client, _k8s_app_config(template_file=str(tpl))
        )

        with pytest.raises(ValueError, match=match):
            provider.create_workload(
                sandbox_id="sbx-1",
                namespace="team-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["python"],
                env={},
                resource_limits={},
                labels={},
                expires_at=None,
                execd_image="opensandbox/execd:test",
                egress_settings=_forward_settings(sandbox_namespace="team-ns"),
            )

        mock_k8s_client.create_custom_object.assert_not_called()

    def test_allows_empty_shard_patches_in_forward_mode(self, tmp_path, mock_k8s_client):
        tpl = tmp_path / "template.yaml"
        tpl.write_text(yaml.safe_dump({"spec": {"shardPatches": []}}))
        provider = BatchSandboxProvider(
            mock_k8s_client, _k8s_app_config(template_file=str(tpl))
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sbx-1", "uid": "uid-1"}
        }

        provider.create_workload(
            sandbox_id="sbx-1",
            namespace="team-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["python"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="opensandbox/execd:test",
            egress_settings=_forward_settings(sandbox_namespace="team-ns"),
        )

        mock_k8s_client.create_custom_object.assert_called_once()

    def test_preserves_host_aliases_template_in_forward_mode(self, tmp_path, mock_k8s_client):
        host_aliases = [{"ip": "10.20.30.40", "hostnames": ["gateway.private"]}]
        tpl = tmp_path / "template.yaml"
        tpl.write_text(
            yaml.safe_dump({"spec": {"template": {"spec": {"hostAliases": host_aliases}}}})
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _k8s_app_config(template_file=str(tpl))
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sbx-1", "uid": "uid-1"}
        }

        provider.create_workload(
            sandbox_id="sbx-1",
            namespace="team-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["python"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="opensandbox/execd:test",
            egress_settings=_forward_settings(
                sandbox_namespace="team-ns",
                upstream_proxy=EgressUpstreamProxyConfig(
                    url="http://gateway.private:3128",
                    authorization="Bearer test-token",
                ),
            ),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        assert pod_spec["hostAliases"] == host_aliases
        egress_env = _env_map(pod_spec["containers"][1])
        assert egress_env["OPENSANDBOX_EGRESS_UPSTREAM_PROXY"] == "http://gateway.private:3128"

    def test_rejects_pool_ref_in_forward_mode(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client, _k8s_app_config())
        req = _create_request(extensions={"poolRef": "warm-pool"})
        with pytest.raises(HTTPException) as exc:
            provider.validate_create_request(req, "opensandbox")
        assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
        assert "poolRef" in exc.value.detail["message"]


class TestAgentSandboxForwardMode:
    def test_attaches_bootstrap_fence_and_rejects_unsafe_templates(
        self, tmp_path, mock_k8s_client
    ):
        provider = AgentSandboxProvider(
            mock_k8s_client, _k8s_app_config(workload_provider="agent-sandbox")
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-sbx-1", "uid": "uid-1"}
        }

        provider.create_workload(
            sandbox_id="sbx-1",
            namespace="team-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["python"],
            env={},
            resource_limits={},
            labels={"opensandbox.io/id": "sbx-1"},
            expires_at=None,
            execd_image="opensandbox/execd:test",
            egress_settings=_forward_settings(
                sandbox_namespace="team-ns", tenant_name="acme"
            ),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["podTemplate"]["spec"]
        assert [c["name"] for c in pod_spec["initContainers"]] == ["execd-installer", "egress-init"]
        assert [c["name"] for c in pod_spec["containers"]] == ["sandbox", "egress"]
        sandbox_c, egress_c = pod_spec["containers"]
        assert set(sandbox_c["securityContext"]["capabilities"]["drop"]) == {"NET_ADMIN", "NET_RAW"}
        egress_env = _env_map(egress_c)
        assert egress_env[OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE] == "team-ns"
        assert egress_env[OPENSANDBOX_EGRESS_TENANT_NAME] == "acme"

        # Template setting shareProcessNamespace or gVisor is rejected before API call.
        for bad_spec, exc_type in [
            ({"shareProcessNamespace": True}, ValueError),
            ({"runtimeClassName": "gvisor"}, HTTPException),
        ]:
            tpl = tmp_path / f"agent-{list(bad_spec)[0]}.yaml"
            tpl.write_text(yaml.safe_dump({"spec": {"podTemplate": {"spec": bad_spec}}}))
            bad_provider = AgentSandboxProvider(
                mock_k8s_client,
                _k8s_app_config(workload_provider="agent-sandbox", template_file=str(tpl)),
            )
            mock_k8s_client.create_custom_object.reset_mock()
            with pytest.raises(exc_type):
                bad_provider.create_workload(
                    sandbox_id="sbx-2",
                    namespace="team-ns",
                    image_spec=ImageSpec(uri="python:3.11"),
                    entrypoint=["python"],
                    env={},
                    resource_limits={},
                    labels={},
                    expires_at=None,
                    execd_image="opensandbox/execd:test",
                    egress_settings=_forward_settings(sandbox_namespace="team-ns"),
                )
            mock_k8s_client.create_custom_object.assert_not_called()


# --- KubernetesSandboxService & CompositeSandboxService -----------------------


def _make_k8s_service(cfg: AppConfig, mock_k8s_client: MagicMock):
    provider = BatchSandboxProvider(mock_k8s_client, cfg)
    with (
        patch("opensandbox_server.services.k8s.kubernetes_service.K8sClient", return_value=mock_k8s_client),
        patch(
            "opensandbox_server.services.k8s.kubernetes_service.create_workload_provider",
            return_value=provider,
        ),
    ):
        svc = KubernetesSandboxService(cfg)
    mock_k8s_client.create_custom_object.return_value = {
        "metadata": {"name": "sbx-1", "uid": "uid-1"}
    }
    return svc


@pytest.mark.asyncio
async def test_k8s_service_single_tenant_sends_namespace_only(mock_k8s_client):
    svc = _make_k8s_service(_k8s_app_config(namespace="single-ns"), mock_k8s_client)
    ready_workload = {
        "metadata": {"name": "sbx-1", "annotations": {}},
        "status": {"replicas": 1, "ready": 1},
    }
    with patch.object(svc, "_wait_for_sandbox_ready", return_value=ready_workload):
        await svc.create_sandbox(_create_request())

    body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
    pod_spec = body["spec"]["template"]["spec"]
    egress_env = _env_map(pod_spec["containers"][1])
    assert egress_env[OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE] == "single-ns"
    assert OPENSANDBOX_EGRESS_TENANT_NAME not in egress_env
    assert [c["name"] for c in pod_spec["initContainers"]] == ["execd-installer", "egress-init"]


@pytest.mark.asyncio
async def test_k8s_service_multi_tenant_sends_distinct_namespace_and_tenant(mock_k8s_client):
    svc = _make_k8s_service(_k8s_app_config(namespace="default-ns"), mock_k8s_client)
    ready_workload = {
        "metadata": {"name": "sbx-1", "annotations": {}},
        "status": {"replicas": 1, "ready": 1},
    }
    set_current_tenant(TenantEntry(name="acme-prod", namespace="shared-tenant-ns"))
    try:
        with patch.object(svc, "_wait_for_sandbox_ready", return_value=ready_workload):
            await svc.create_sandbox(_create_request())
    finally:
        set_current_tenant(None)

    body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
    egress_env = _env_map(body["spec"]["template"]["spec"]["containers"][1])
    assert egress_env[OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE] == "shared-tenant-ns"
    assert egress_env[OPENSANDBOX_EGRESS_TENANT_NAME] == "acme-prod"


@pytest.mark.asyncio
@pytest.mark.parametrize("bad_name", ["acme corp", "acme\r\nX-Evil: 1", "租户"])
async def test_k8s_service_rejects_invalid_identity_values_fail_closed(
    mock_k8s_client, bad_name
):
    svc = _make_k8s_service(_k8s_app_config(namespace="default-ns"), mock_k8s_client)
    set_current_tenant(TenantEntry(name=bad_name, namespace="team-ns"))
    try:
        with pytest.raises(HTTPException) as exc:
            await svc.create_sandbox(_create_request())
    finally:
        set_current_tenant(None)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    mock_k8s_client.create_custom_object.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "overrides",
    [
        {"extensions": {"poolRef": "warm-pool"}},
        {"networkPolicy": NetworkPolicy(default_action="deny", egress=[])},
        {
            "networkPolicy": NetworkPolicy(default_action="deny", egress=[]),
            "credentialProxy": CredentialProxyConfig(enabled=True),
        },
        {"env": {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true"}},
        {"env": {"OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE": "spoofed"}},
        {"env": {"OPENSANDBOX_EGRESS_TENANT_NAME": "spoofed"}},
        {"platform": PlatformSpec(os="windows", arch="amd64")},
    ],
)
async def test_k8s_service_rejects_disallowed_forward_requests(mock_k8s_client, overrides):
    svc = _make_k8s_service(_k8s_app_config(), mock_k8s_client)
    with pytest.raises(HTTPException) as exc:
        await svc.create_sandbox(_create_request(**overrides))

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    mock_k8s_client.create_custom_object.assert_not_called()


@pytest.mark.asyncio
async def test_composite_service_rejects_fast_sandbox_in_forward_mode(mock_k8s_client):
    cfg = _k8s_app_config()
    k8s_svc = _make_k8s_service(cfg, mock_k8s_client)
    fsb_svc = FastSandboxService(cfg, k8s_client=mock_k8s_client)
    composite = CompositeSandboxService(k8s_svc, fsb_svc)

    req = CreateSandboxRequest(
        templateId="tpl-python",
        timeout=120,
    )
    with pytest.raises(HTTPException) as exc:
        await composite.create_sandbox(req)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert "fast sandboxes" in exc.value.detail["message"]
