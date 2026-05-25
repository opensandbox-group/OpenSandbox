# Copyright 2025 Alibaba Group Holding Ltd.
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
from datetime import datetime
from typing import Dict, List, Any, Optional

from opensandbox_server.api.schema import Endpoint, ImageSpec, NetworkPolicy, PlatformSpec, Volume
from opensandbox_server.config import EGRESS_MODE_DNS


class WorkloadProvider(ABC):
    """
    Abstract interface for managing Kubernetes workload resources.
    
    This abstraction allows supporting different K8s resource types
    (Pod, Job, StatefulSet, etc.) with a unified interface.
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
        network_policy: Optional[NetworkPolicy] = None,
        egress_image: Optional[str] = None,
        volumes: Optional[List[Volume]] = None,
        platform: Optional[PlatformSpec] = None,
        annotations: Optional[Dict[str, str]] = None,
        egress_auth_token: Optional[str] = None,
        egress_mode: str = EGRESS_MODE_DNS,
    ) -> Dict[str, Any]:
        """
        Create a new workload resource.

        Args:
            sandbox_id: Unique sandbox identifier
            namespace: Kubernetes namespace
            image_spec: Container image specification
            entrypoint: Container entrypoint command
            env: Environment variables
            resource_limits: Resource limits (cpu, memory)
            labels: Labels to apply to the workload
            expires_at: Expiration time, or None for manual cleanup (no TTL)
            execd_image: execd daemon image
            extensions: General extension field for passing additional configuration.
                This is a flexible field for various use cases (e.g., ``poolRef`` for pool-based creation).
            network_policy: Optional network policy for egress traffic control.
                When provided, an egress sidecar container will be added to the Pod.
            egress_image: Optional egress sidecar image. Required when network_policy is provided.
            egress_mode: Sidecar ``OPENSANDBOX_EGRESS_MODE`` (from app ``[egress].mode`` when using network policy).
            volumes: Optional list of volume mounts for the sandbox.

        Returns:
            Dict containing workload metadata (name, uid, etc.)

        Raises:
            ApiException: If creation fails
        """
        pass
    
    @abstractmethod
    def get_workload(self, sandbox_id: str, namespace: str) -> Optional[Any]:
        """
        Get workload by sandbox ID.
        
        Args:
            sandbox_id: Unique sandbox identifier
            namespace: Kubernetes namespace
            
        Returns:
            Workload object or None if not found
        """
        pass
    
    @abstractmethod
    def delete_workload(self, sandbox_id: str, namespace: str) -> None:
        """
        Delete a workload resource.

        Args:
            sandbox_id: Unique sandbox identifier
            namespace: Kubernetes namespace

        Raises:
            ApiException: If deletion fails
        """
        pass
    
    @abstractmethod
    def list_workloads(self, namespace: str, label_selector: str) -> List[Any]:
        """
        List workloads matching label selector.
        
        Args:
            namespace: Kubernetes namespace
            label_selector: Label selector query
            
        Returns:
            List of workload objects
        """
        pass
    
    @abstractmethod
    def update_expiration(self, sandbox_id: str, namespace: str, expires_at: datetime) -> None:
        """
        Update workload expiration time.
        
        Args:
            sandbox_id: Unique sandbox identifier
            namespace: Kubernetes namespace
            expires_at: New expiration time
            
        Raises:
            Exception: If update fails
        """
        pass
    
    @abstractmethod
    def get_expiration(self, workload: Any) -> Optional[datetime]:
        """
        Get expiration time from workload.
        
        Args:
            workload: Workload object
            
        Returns:
            Expiration datetime or None if not set
        """
        pass
    
    @abstractmethod
    def get_status(self, workload: Any) -> Dict[str, Any]:
        """
        Get status from workload object.
        
        Args:
            workload: Workload object
            
        Returns:
            Dict with state, reason, message, last_transition_at
        """
        pass
    
    @abstractmethod
    def get_endpoint_info(self, workload: Any, port: int, sandbox_id: str) -> Optional[Endpoint]:
        """
        Get endpoint information from workload.

        Args:
            workload: Workload object
            port: Port number
            sandbox_id: Sandbox identifier for ingress-based endpoints

        Returns:
            Endpoint object (including optional headers) or None if not available
        """
        pass

    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """
        Pause a running sandbox.

        The provider validates the current state and signals the pause intent.
        Raises NotImplementedError if the provider does not support pause.
        Raises ValueError if the sandbox is in an invalid state for pause.
        Raises Exception if the sandbox is not found or the API call fails.
        """
        raise NotImplementedError("Pause is not supported by this provider")

    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """
        Resume a paused sandbox.

        The provider validates the current state and signals the resume intent.
        Raises NotImplementedError if the provider does not support resume.
        Raises ValueError if the sandbox is in an invalid state for resume.
        Raises Exception if the sandbox is not found or the API call fails.
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
    def has_platform_constraints_in_pod_spec(pod_spec: Any) -> bool:
        has_platform, _ = WorkloadProvider.analyze_platform_constraints_in_pod_spec(pod_spec)
        return has_platform

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
