# Copyright 2025 The OpenSandbox Authors
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
Abstract workload provider interface for Kubernetes resources.
"""

from abc import ABC, abstractmethod
from dataclasses import dataclass
from datetime import datetime
from typing import Callable, Dict, List, Any, Optional

from opensandbox_server.api.schema import Endpoint, ImageSpec, NetworkPolicy, PlatformSpec, Volume
from opensandbox_server.config import EgressUpstreamProxyConfig


@dataclass(frozen=True)
class EgressWorkloadSettings:
    """All server- and request-derived settings needed to build an egress sidecar."""

    network_policy: NetworkPolicy
    image: str
    mode: str
    auth_token: Optional[str]
    credential_proxy_enabled: bool
    env: Dict[str, Optional[str]]
    disable_ipv6: bool
    resource_requests: Optional[Dict[str, str]]
    resource_limits: Optional[Dict[str, str]]
    otlp_endpoint: Optional[str] = None
    upstream_proxy: Optional[EgressUpstreamProxyConfig] = None


class WorkloadProvider(ABC):
    """
    Abstract interface for managing Kubernetes workload resources.

    This abstraction allows supporting different workload CRDs
    (BatchSandbox, agent-sandbox Sandbox, etc.) with a unified interface.
    """
    
    @abstractmethod
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
        egress_settings: Optional[EgressWorkloadSettings] = None,
        volumes: Optional[List[Volume]] = None,
        platform: Optional[PlatformSpec] = None,
        annotations: Optional[Dict[str, str]] = None,
        resource_requests: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        """
        Create a new workload resource, raising ApiException on failure.

        ``expires_at=None`` means no TTL (manual cleanup). ``resource_requests``
        defaults to ``resource_limits`` when omitted. ``egress_settings`` is None
        when no egress policy enforcement is needed. ``extensions`` is a general
        field for provider extras (e.g. ``poolRef`` for pool-based creation).
        """
        pass
    
    def subscribe_workload(
        self, sandbox_id: str, namespace: str, callback: Callable[[str, Dict[str, Any]], None]
    ) -> Optional[Callable[[], None]]:
        """Return an unsubscribe callback, or None to use periodic checks only."""
        return None

    @abstractmethod
    def get_workload(self, sandbox_id: str, namespace: str) -> Optional[Any]:
        """Get workload by sandbox ID; returns None when not found."""
        pass
    
    @abstractmethod
    def delete_workload(self, sandbox_id: str, namespace: str) -> None:
        """Delete a workload resource, raising ApiException on failure."""
        pass
    
    @abstractmethod
    def list_workloads(self, namespace: str, label_selector: str) -> List[Any]:
        """List workloads matching the label selector."""
        pass
    
    def list_workloads_all_namespaces(self, label_selector: str) -> List[Any]:
        """
        List workloads matching label selector across all namespaces.

        Used by the cross-namespace sandbox lookup fallback. Providers
        without cluster-wide read access may keep this default; the
        fallback then simply finds nothing.

        Args:
            label_selector: Label selector query

        Returns:
            List of workload objects
        """
        return []
    
    @abstractmethod
    def update_expiration(self, sandbox_id: str, namespace: str, expires_at: datetime) -> None:
        """Update workload expiration time, raising if the update fails."""
        pass
    
    @abstractmethod
    def get_expiration(self, workload: Any) -> Optional[datetime]:
        """Get expiration time from workload, or None if not set."""
        pass
    
    @abstractmethod
    def get_status(self, workload: Any) -> Dict[str, Any]:
        """Return a dict with ``state``, ``reason``, ``message``, ``last_transition_at``."""
        pass
    
    @abstractmethod
    def get_endpoint_info(self, workload: Any, port: int, sandbox_id: str) -> Optional[Endpoint]:
        """Return the sandbox endpoint, or None when not available."""
        pass

    def get_internal_endpoint(self, workload: Any, port: int, sandbox_id: str) -> Optional[Endpoint]:
        """Get the workload endpoint used by the server-side proxy.

        Providers with ingress-aware endpoint resolution should override this
        method to return an address that bypasses ingress. The default preserves
        compatibility for custom providers that only implement get_endpoint_info.
        """
        return self.get_endpoint_info(workload, port, sandbox_id)

    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """
        Pause a running sandbox; raises NotImplementedError when unsupported
        and ValueError when the current state does not allow pause.
        """
        raise NotImplementedError("Pause is not supported by this provider")

    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """
        Resume a paused sandbox; raises NotImplementedError when unsupported
        and ValueError when the current state does not allow resume.
        """
        raise NotImplementedError("Resume is not supported by this provider")

    def patch_labels(
        self, name: str, namespace: str, labels: Dict[str, Optional[str]]
    ) -> Dict[str, Any]:
        """Patch workload metadata.labels via JSON merge patch.

        A None value for a label key deletes that label per RFC 7396.
        Returns the API server response (the patched workload).
        """
        body = {"metadata": {"labels": labels}}
        return self.k8s_client.patch_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            name=name,
            body=body,
        )

    def supports_image_auth(self) -> bool:
        """
        Whether this provider supports per-request image pull authentication.

        Providers that implement imagePullSecrets injection should override
        this method to return True.
        """
        return False

    def legacy_resource_name(self, sandbox_id: str) -> str:
        """
        Convert a sandbox_id to the legacy resource name with prefix.

        Pre-upgrade sandboxes were named ``sandbox-<id>``. This helper
        preserves access to those resources while allowing plain IDs
        for new ones.
        """
        if sandbox_id.startswith("sandbox-"):
            return sandbox_id
        return f"sandbox-{sandbox_id}"

    @staticmethod
    def is_unschedulable_reason(reason: Optional[str]) -> bool:
        if not isinstance(reason, str):
            return False
        normalized = reason.strip().lower()
        return normalized in {"unschedulable", "failedscheduling"} or "unschedulable" in normalized

    @staticmethod
    def analyze_platform_constraints_in_pod_spec(pod_spec: Any) -> tuple[bool, bool]:
        """
        Analyze pod scheduling constraints and return:
        - has_platform_constraints: whether kubernetes.io/os|arch constraints exist
        - has_non_platform_constraints: whether any other selector/affinity constraints exist
        """
        if not isinstance(pod_spec, dict):
            return False, False
        has_platform_constraints = False
        has_non_platform_constraints = False

        node_selector = pod_spec.get("nodeSelector", {})
        if isinstance(node_selector, dict):
            for key in node_selector.keys():
                if key in ("kubernetes.io/os", "kubernetes.io/arch"):
                    has_platform_constraints = True
                else:
                    has_non_platform_constraints = True

        affinity = pod_spec.get("affinity", {})
        if not isinstance(affinity, dict):
            return has_platform_constraints, has_non_platform_constraints
        node_affinity = affinity.get("nodeAffinity", {})
        if not isinstance(node_affinity, dict):
            return has_platform_constraints, has_non_platform_constraints
        required = node_affinity.get("requiredDuringSchedulingIgnoredDuringExecution", {})
        if not isinstance(required, dict):
            return has_platform_constraints, has_non_platform_constraints
        terms = required.get("nodeSelectorTerms", [])
        if not isinstance(terms, list):
            return has_platform_constraints, has_non_platform_constraints
        for term in terms:
            expressions = term.get("matchExpressions", []) if isinstance(term, dict) else []
            if not isinstance(expressions, list):
                continue
            for expr in expressions:
                key = expr.get("key") if isinstance(expr, dict) else None
                if key in ("kubernetes.io/os", "kubernetes.io/arch"):
                    has_platform_constraints = True
                elif isinstance(key, str) and key:
                    has_non_platform_constraints = True
        return has_platform_constraints, has_non_platform_constraints

    @staticmethod
    def apply_platform_node_selector(
        pod_spec: Dict[str, Any],
        template_spec: Dict[str, Any],
        platform: Optional[PlatformSpec],
    ) -> None:
        if platform is None:
            return

        template_selector = template_spec.get("nodeSelector", {})
        if not isinstance(template_selector, dict):
            template_selector = {}

        requested = {
            "kubernetes.io/os": platform.os,
            "kubernetes.io/arch": platform.arch,
        }
        for key, value in requested.items():
            existing = template_selector.get(key)
            if existing is not None and existing != value:
                raise ValueError(
                    f"platform conflict with template nodeSelector: '{key}' "
                    f"is '{existing}', request expects '{value}'."
                )

        WorkloadProvider.ensure_platform_compatible_with_affinity(template_spec, platform)

        node_selector = pod_spec.setdefault("nodeSelector", {})
        if not isinstance(node_selector, dict):
            node_selector = {}
            pod_spec["nodeSelector"] = node_selector
        node_selector.update(requested)

    @staticmethod
    def ensure_platform_compatible_with_affinity(
        pod_spec: Dict[str, Any],
        platform: PlatformSpec,
    ) -> None:
        affinity = pod_spec.get("affinity", {})
        if not isinstance(affinity, dict):
            return

        node_affinity = affinity.get("nodeAffinity", {})
        if not isinstance(node_affinity, dict):
            return

        required = node_affinity.get("requiredDuringSchedulingIgnoredDuringExecution", {})
        if not isinstance(required, dict):
            return

        terms = required.get("nodeSelectorTerms", [])
        if not isinstance(terms, list) or not terms:
            return

        requested = {
            "kubernetes.io/os": platform.os,
            "kubernetes.io/arch": platform.arch,
        }
        if any(
            WorkloadProvider.node_selector_term_satisfiable(term, requested)
            for term in terms
            if isinstance(term, dict)
        ):
            return

        raise ValueError(
            "platform conflict with template nodeAffinity: required node affinity "
            f"does not allow requested platform '{platform.os}/{platform.arch}'."
        )

    @staticmethod
    def node_selector_term_satisfiable(
        term: Dict[str, Any],
        requested: Dict[str, str],
    ) -> bool:
        expressions = term.get("matchExpressions", [])
        if not isinstance(expressions, list):
            expressions = []

        for expr in expressions:
            if not isinstance(expr, dict):
                continue
            key = expr.get("key")
            if key not in requested:
                continue
            operator = expr.get("operator")
            values = expr.get("values", [])
            if not isinstance(values, list):
                values = []
            value = requested[key]

            if operator == "In" and value not in values:
                return False
            if operator == "NotIn" and value in values:
                return False
            if operator == "DoesNotExist":
                return False

        return True

    @staticmethod
    def is_platform_unschedulable(
        reason: Optional[str],
        message: Optional[str],
        workload_has_platform_constraints: bool,
        workload_has_non_platform_constraints: bool = False,
    ) -> bool:
        if not workload_has_platform_constraints:
            return False
        if not WorkloadProvider.is_unschedulable_reason(reason):
            return False
        if not isinstance(message, str):
            return False
        normalized = message.lower()
        transient_capacity_indicators = (
            "insufficient cpu",
            "insufficient memory",
            "insufficient ephemeral-storage",
            "too many pods",
            "insufficient pods",
            "preemption is not helpful",
            "preemption: 0/",
        )
        if any(indicator in normalized for indicator in transient_capacity_indicators):
            return False
        if "kubernetes.io/os" in normalized or "kubernetes.io/arch" in normalized:
            return True
        # kube-scheduler often emits generic affinity/selector mismatch text
        # without explicit label keys when nodeSelector/nodeAffinity is unsatisfied.
        # Only treat this as platform-related when there are no non-platform
        # selector/affinity keys that could have caused the mismatch.
        if workload_has_non_platform_constraints:
            return False
        standard_patterns = (
            "didn't match pod's node affinity",
            "didn't match pod's node selector",
            "did not match pod's node affinity",
            "did not match pod's node selector",
        )
        return any(pattern in normalized for pattern in standard_patterns)
