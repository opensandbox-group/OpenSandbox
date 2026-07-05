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

import json
from typing import Optional

import pytest

from opensandbox_server.api.schema import NetworkPolicy, NetworkRule
from opensandbox_server.config import EGRESS_MODE_DNS, EGRESS_MODE_DNS_NFT
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RULES_ENV,
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT,
    OPENSANDBOX_EGRESS_TOKEN,
    OPENSANDBOX_RUNTIME_MOUNT_PATH,
    OPENSANDBOX_RUNTIME_VOLUME_NAME,
)
from opensandbox_server.services.helpers import split_egress_env
from opensandbox_server.services.k8s.egress_helper import (
    apply_egress_to_spec,
    build_security_context_for_sandbox_container,
    prep_execd_init_for_egress,
)


def _egress_container(
    egress_image: str,
    network_policy: NetworkPolicy,
    *,
    egress_auth_token: Optional[str] = None,
    egress_mode: str = EGRESS_MODE_DNS,
    credential_proxy_enabled: bool = False,
) -> dict:
    """Sidecar dict produced by ``apply_egress_to_spec``."""
    containers: list = []
    apply_egress_to_spec(
        containers,
        network_policy,
        egress_image,
        egress_auth_token=egress_auth_token,
        egress_mode=egress_mode,
        credential_proxy_enabled=credential_proxy_enabled,
    )
    return containers[0]


class TestEgressSidecarViaApply:
    """Egress sidecar shape (via ``apply_egress_to_spec``)."""

    def test_builds_container_with_basic_config(self):
        """Test that container is built with correct basic configuration."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[
                NetworkRule(action="allow", target="pypi.org"),
            ],
        )

        container = _egress_container(egress_image, network_policy)

        assert container["name"] == "egress"
        assert container["image"] == egress_image
        assert "env" in container
        assert "securityContext" in container

    def test_contains_egress_rules_environment_variable(self):
        """Test that container includes OPENSANDBOX_EGRESS_RULES environment variable."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(egress_image, network_policy)

        env_vars = container["env"]
        env_by_name = {env["name"]: env["value"] for env in env_vars}
        assert env_by_name[EGRESS_RULES_ENV] is not None
        assert env_by_name[EGRESS_MODE_ENV] == EGRESS_MODE_DNS
        assert OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT not in env_by_name

    def test_always_mounts_runtime_volume(self):
        container = _egress_container(
            "opensandbox/egress:v1.1.3",
            NetworkPolicy(default_action="deny", egress=[]),
        )
        assert container["volumeMounts"] == [
            {
                "name": OPENSANDBOX_RUNTIME_VOLUME_NAME,
                "mountPath": OPENSANDBOX_RUNTIME_MOUNT_PATH,
            }
        ]

    def test_contains_transparent_mitm_env_when_credential_proxy_enabled(self):
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(
            egress_image,
            network_policy,
            credential_proxy_enabled=True,
        )

        env_by_name = {env["name"]: env["value"] for env in container["env"]}
        assert env_by_name[OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT] == "true"
        assert container["volumeMounts"] == [
            {
                "name": OPENSANDBOX_RUNTIME_VOLUME_NAME,
                "mountPath": OPENSANDBOX_RUNTIME_MOUNT_PATH,
            }
        ]

    def test_contains_egress_token_when_provided(self):
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(
            egress_image,
            network_policy,
            egress_auth_token="egress-token",
        )

        env_vars = {env["name"]: env["value"] for env in container["env"]}
        assert env_vars[OPENSANDBOX_EGRESS_TOKEN] == "egress-token"
        assert env_vars[EGRESS_MODE_ENV] == EGRESS_MODE_DNS
        assert container["readinessProbe"]["httpGet"]["httpHeaders"] == [
            {"name": OPEN_SANDBOX_EGRESS_AUTH_HEADER, "value": "egress-token"}
        ]

    def test_egress_mode_dns_nft(self):
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(
            egress_image,
            network_policy,
            egress_mode=EGRESS_MODE_DNS_NFT,
        )

        env_vars = {env["name"]: env["value"] for env in container["env"]}
        assert env_vars[EGRESS_MODE_ENV] == EGRESS_MODE_DNS_NFT

    def test_serializes_network_policy_correctly(self):
        """Test that network policy is correctly serialized to JSON."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[
                NetworkRule(action="allow", target="pypi.org"),
                NetworkRule(action="deny", target="*.malicious.com"),
            ],
        )

        container = _egress_container(egress_image, network_policy)

        env_value = container["env"][0]["value"]
        policy_dict = json.loads(env_value)

        assert "defaultAction" in policy_dict
        assert policy_dict["defaultAction"] == "deny"
        assert "egress" in policy_dict
        assert len(policy_dict["egress"]) == 2
        assert policy_dict["egress"][0]["action"] == "allow"
        assert policy_dict["egress"][0]["target"] == "pypi.org"
        assert policy_dict["egress"][1]["action"] == "deny"
        assert policy_dict["egress"][1]["target"] == "*.malicious.com"

    def test_handles_empty_egress_rules(self):
        """Test that empty egress rules are handled correctly."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="allow",
            egress=[],
        )

        container = _egress_container(egress_image, network_policy)

        env_value = container["env"][0]["value"]
        policy_dict = json.loads(env_value)

        assert policy_dict["defaultAction"] == "allow"
        assert policy_dict["egress"] == []

    def test_handles_missing_default_action(self):
        """Test that missing default_action is handled (exclude_none=True)."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(egress_image, network_policy)

        env_value = container["env"][0]["value"]
        policy_dict = json.loads(env_value)

        assert "defaultAction" not in policy_dict or policy_dict.get("defaultAction") is None
        assert "egress" in policy_dict

    def test_security_context_adds_net_admin_not_privileged(self):
        """Egress sidecar uses NET_ADMIN only (IPv6 is disabled in execd init when egress is on)."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[],
        )

        container = _egress_container(egress_image, network_policy)

        security_context = container["securityContext"]
        assert security_context.get("privileged") is not True
        assert "NET_ADMIN" in security_context.get("capabilities", {}).get("add", [])

    def test_no_command_uses_image_entrypoint(self):
        container = _egress_container(
            "opensandbox/egress:v1.1.3",
            NetworkPolicy(default_action="deny", egress=[]),
        )
        assert "command" not in container

    def test_container_spec_is_valid_kubernetes_format(self):
        """Test that returned container spec is in valid Kubernetes format."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        container = _egress_container(egress_image, network_policy)

        assert "name" in container
        assert "image" in container
        assert "env" in container
        assert "securityContext" in container

        assert isinstance(container["env"], list)
        assert len(container["env"]) > 0
        assert "name" in container["env"][0]
        assert "value" in container["env"][0]
        assert "command" not in container
        assert container["ports"] == [{"name": "egress-api", "containerPort": 18080}]
        assert container["readinessProbe"]["httpGet"]["path"] == "/healthz"

    def test_handles_wildcard_domains(self):
        """Test that wildcard domains in egress rules are handled correctly."""
        egress_image = "opensandbox/egress:v1.1.3"
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[
                NetworkRule(action="allow", target="*.python.org"),
                NetworkRule(action="allow", target="pypi.org"),
            ],
        )

        container = _egress_container(egress_image, network_policy)

        env_value = container["env"][0]["value"]
        policy_dict = json.loads(env_value)

        assert len(policy_dict["egress"]) == 2
        assert policy_dict["egress"][0]["target"] == "*.python.org"
        assert policy_dict["egress"][1]["target"] == "pypi.org"


class TestBuildSecurityContextForMainContainer:
    def test_returns_empty_dict_when_no_network_policy(self):
        """Test that empty dict is returned when network policy is disabled."""
        result = build_security_context_for_sandbox_container(has_network_policy=False)
        assert result == {}

    def test_drops_net_admin_when_network_policy_enabled(self):
        """Test that NET_ADMIN is dropped when network policy is enabled."""
        result = build_security_context_for_sandbox_container(has_network_policy=True)

        assert "capabilities" in result
        assert "drop" in result["capabilities"]
        assert "NET_ADMIN" in result["capabilities"]["drop"]


class TestApplyEgressToSpec:
    def test_adds_egress_sidecar_container(self):
        """Test that egress sidecar container is added to containers list."""
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )
        egress_image = "opensandbox/egress:v1.1.3"

        apply_egress_to_spec(
            containers,
            network_policy,
            egress_image,
        )

        assert len(containers) == 1
        assert containers[0]["name"] == "egress"
        assert containers[0]["image"] == egress_image

    def test_does_not_touch_unrelated_pod_state(self):
        """apply_egress_to_spec only appends to containers (no pod_spec parameter)."""
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )
        egress_image = "opensandbox/egress:v1.1.3"

        apply_egress_to_spec(
            containers,
            network_policy,
            egress_image,
        )

        assert len(containers) == 1

    def test_preserves_existing_pod_sysctls_when_not_passed_in(self):
        """Callers keep pod sysctls in their own dict; apply does not mutate them."""
        pod_spec: dict = {
            "securityContext": {
                "sysctls": [
                    {"name": "net.core.somaxconn", "value": "1024"},
                    {"name": "net.ipv6.conf.all.disable_ipv6", "value": "0"},
                ]
            }
        }
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )
        egress_image = "opensandbox/egress:v1.1.3"

        apply_egress_to_spec(
            containers,
            network_policy,
            egress_image,
        )

        sysctls = pod_spec["securityContext"]["sysctls"]
        sysctl_dict = {s["name"]: s["value"] for s in sysctls}

        assert sysctl_dict["net.core.somaxconn"] == "1024"
        assert sysctl_dict["net.ipv6.conf.all.disable_ipv6"] == "0"
        assert len(sysctls) == 2

    def test_no_op_when_no_network_policy(self):
        """Test that function does nothing when network_policy is None."""
        containers: list = []

        apply_egress_to_spec(
            containers,
            None,
            "opensandbox/egress:v1.1.3",
        )

        assert len(containers) == 0

    def test_no_op_when_no_egress_image(self):
        """Test that function does nothing when egress_image is None."""
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        apply_egress_to_spec(
            containers,
            network_policy,
            None,
        )

        assert len(containers) == 0

    def test_extra_env_injected_into_sidecar(self):
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )
        extra = {
            "OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT": "/scripts/auth.py",
            "OPENSANDBOX_EGRESS_LOG_LEVEL": "debug",
        }

        apply_egress_to_spec(
            containers,
            network_policy,
            "opensandbox/egress:v1.1.3",
            extra_env=extra,
        )

        env_by_name = {e["name"]: e["value"] for e in containers[0]["env"]}
        assert env_by_name["OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT"] == "/scripts/auth.py"
        assert env_by_name["OPENSANDBOX_EGRESS_LOG_LEVEL"] == "debug"

    def test_extra_env_none_value_becomes_empty_string(self):
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        apply_egress_to_spec(
            containers,
            network_policy,
            "opensandbox/egress:v1.1.3",
            extra_env={"OPENSANDBOX_EGRESS_LOG_LEVEL": None},
        )

        env_by_name = {e["name"]: e["value"] for e in containers[0]["env"]}
        assert env_by_name["OPENSANDBOX_EGRESS_LOG_LEVEL"] == ""

    def test_extra_env_mitm_ignored_when_credential_proxy_enabled(self):
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        apply_egress_to_spec(
            containers,
            network_policy,
            "opensandbox/egress:v1.1.3",
            credential_proxy_enabled=True,
            extra_env={"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "false"},
        )

        mitm_vals = [
            e["value"]
            for e in containers[0]["env"]
            if e["name"] == OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT
        ]
        assert mitm_vals == ["true"]

    def test_extra_env_empty_dict_is_noop(self):
        containers: list = []
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        apply_egress_to_spec(
            containers,
            network_policy,
            "opensandbox/egress:v1.1.3",
            extra_env={},
        )

        env_names = {e["name"] for e in containers[0]["env"]}
        assert env_names == {EGRESS_RULES_ENV, EGRESS_MODE_ENV}


class TestPrepExecdInitForEgress:
    def test_returns_privileged_security_dict_and_prefixed_script(self):
        base = "cp ./execd /opt/opensandbox/execd"
        script, sc = prep_execd_init_for_egress(base)
        assert sc == {"privileged": True}
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" in script
        assert script.endswith(base)


class TestSplitEgressEnv:
    def test_splits_by_prefix(self):
        env = {
            "MY_VAR": "hello",
            "OPENSANDBOX_EGRESS_LOG_LEVEL": "debug",
            "OTHER": "world",
        }
        sandbox_env, egress_env = split_egress_env(env)
        assert sandbox_env == {"MY_VAR": "hello", "OTHER": "world"}
        assert egress_env == {"OPENSANDBOX_EGRESS_LOG_LEVEL": "debug"}

    def test_none_returns_empty_dicts(self):
        sandbox_env, egress_env = split_egress_env(None)
        assert sandbox_env == {}
        assert egress_env == {}

    def test_empty_returns_empty_dicts(self):
        sandbox_env, egress_env = split_egress_env({})
        assert sandbox_env == {}
        assert egress_env == {}

    def test_no_egress_vars(self):
        env = {"FOO": "bar", "BAZ": "qux"}
        sandbox_env, egress_env = split_egress_env(env)
        assert sandbox_env == env
        assert egress_env == {}

    def test_rejects_disallowed_rules(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_RULES": "evil"})

    def test_rejects_disallowed_mode(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_MODE": "evil"})

    def test_rejects_disallowed_token(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_TOKEN": "evil"})

    def test_rejects_disallowed_http_addr(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_HTTP_ADDR": "0.0.0.0:9999"})

    def test_rejects_disallowed_dns_upstream(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_DNS_UPSTREAM": "8.8.8.8"})

    def test_rejects_disallowed_nameserver_exempt(self):
        with pytest.raises(ValueError, match="not allowed"):
            split_egress_env({"OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT": "1.1.1.1"})

    def test_allows_mitmproxy_transparent(self):
        env = {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true"}
        sandbox_env, egress_env = split_egress_env(env)
        assert sandbox_env == {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true"}
        assert egress_env == {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true"}

    def test_allows_all_permitted_vars(self):
        from opensandbox_server.services.constants import ALLOWED_EGRESS_ENV_VARS
        env = {key: "val" for key in ALLOWED_EGRESS_ENV_VARS}
        sandbox_env, egress_env = split_egress_env(env)
        assert set(egress_env.keys()) == ALLOWED_EGRESS_ENV_VARS
