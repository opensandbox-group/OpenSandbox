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
BatchSandbox-based workload provider implementation.
"""

import logging
import json
import shlex
from datetime import datetime
from typing import Dict, List, Any, Optional

from opensandbox_server.config import (
    AppConfig,
    DEFAULT_EGRESS_DISABLE_IPV6,
    EGRESS_MODE_DNS,
    INGRESS_MODE_GATEWAY,
)
from opensandbox_server.services.helpers import format_ingress_endpoint
from opensandbox_server.api.schema import Endpoint, ImageSpec, NetworkPolicy, PlatformSpec, Volume
from opensandbox_server.services.k8s.image_pull_secret_helper import (
    build_image_pull_secret,
    build_image_pull_secret_name,
)
from opensandbox_server.services.k8s.batchsandbox_template import BatchSandboxTemplateManager
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.egress_helper import (
    apply_egress_to_spec,
)
from opensandbox_server.services.k8s.provider_common import (
    DEFAULT_ENTRYPOINT,
    _build_execd_init_container,
    _build_main_container,
    _container_to_dict,
    _extract_platform_unschedulable_message_from_pod,
    _workload_platform_constraint_scope,
)
from opensandbox_server.services.k8s.windows_profile import (
    apply_windows_profile_arch_selector,
    apply_windows_profile_overrides,
    is_windows_profile,
    validate_windows_profile_resource_limits,
)
from opensandbox_server.services.k8s.volume_helper import apply_volumes_to_pod_spec
from opensandbox_server.services.k8s.workload_provider import WorkloadProvider
from opensandbox_server.services.runtime_resolver import SecureRuntimeResolver

logger = logging.getLogger(__name__)


class BatchSandboxProvider(WorkloadProvider):
    """Workload provider for BatchSandbox CRDs."""
    
    def __init__(
        self,
        k8s_client: K8sClient,
        app_config: Optional[AppConfig] = None,
    ):
        self.k8s_client = k8s_client
        self.ingress_config = app_config.ingress if app_config else None

        k8s_config = app_config.kubernetes if app_config else None
        template_file_path = k8s_config.batchsandbox_template_file if k8s_config else None
        if template_file_path:
            logger.info(f"Using BatchSandbox template file: {template_file_path}")
        self.execd_init_resources = k8s_config.execd_init_resources if k8s_config else None
        self.image_pull_policy = k8s_config.image_pull_policy if k8s_config else "IfNotPresent"

        self.resolver = SecureRuntimeResolver(app_config) if app_config else None
        self.runtime_class = (
            self.resolver.get_k8s_runtime_class() if self.resolver else None
        )

        self.group = "sandbox.opensandbox.io"
        self.version = "v1alpha1"
        self.plural = "batchsandboxes"

        self.template_manager = BatchSandboxTemplateManager(template_file_path)

        self.egress_disable_ipv6 = (
            bool(app_config.egress.disable_ipv6)
            if app_config and app_config.egress is not None
            else DEFAULT_EGRESS_DISABLE_IPV6
        )

    def supports_image_auth(self) -> bool:
        """BatchSandbox supports per-request image pull auth."""
        return True

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
        """Create a BatchSandbox in template mode or pool mode."""
        extensions = extensions or {}
        windows_profile = is_windows_profile(platform)

        if self.runtime_class:
            logger.info(f"Using Kubernetes RuntimeClass '{self.runtime_class}' for sandbox {sandbox_id}")

        if extensions.get("poolRef"):
            if platform is not None:
                raise ValueError(
                    "platform is not supported together with extensions.poolRef yet. "
                    "Pool-level platform modeling is not available in this iteration."
                )
            if volumes:
                raise ValueError(
                    "Pool mode does not support volumes. "
                    "Remove 'volumes' from request or use template mode."
                )
            return self._create_workload_from_pool(
                batchsandbox_name=sandbox_id,
                namespace=namespace,
                labels=labels,
                pool_ref=extensions["poolRef"],
                expires_at=expires_at,
                entrypoint=entrypoint,
                env=env,
                annotations=annotations,
            )

        extra_volumes, extra_mounts = self._extract_template_pod_extras()

        if windows_profile:
            validate_windows_profile_resource_limits(resource_limits)

        disable_ipv6_for_egress = (
            network_policy is not None
            and egress_image is not None
            and self.egress_disable_ipv6
        )
        init_container = _build_execd_init_container(
            execd_image,
            self.execd_init_resources,
            disable_ipv6_for_egress=disable_ipv6_for_egress,
        )
        
        main_container = _build_main_container(
            image_spec=image_spec,
            entrypoint=entrypoint,
            env=env,
            resource_limits=resource_limits,
            has_network_policy=network_policy is not None,
            image_pull_policy=self.image_pull_policy,
        )
        
        containers = [_container_to_dict(main_container)]
        pod_spec = {
            "initContainers": [_container_to_dict(init_container)],
            "containers": containers,
            "volumes": [
                {
                    "name": "opensandbox-bin",
                    "emptyDir": {}
                }
            ],
        }
        if windows_profile:
            apply_windows_profile_overrides(
                pod_spec=pod_spec,
                entrypoint=entrypoint,
                env=env,
                resource_limits=resource_limits,
                disable_ipv6_for_egress=disable_ipv6_for_egress,
            )
            template = self.template_manager.get_base_template()
            template_spec = (
                template.get("spec", {})
                .get("template", {})
                .get("spec", {})
            )
            apply_windows_profile_arch_selector(
                pod_spec=pod_spec,
                template_spec=template_spec if isinstance(template_spec, dict) else {},
                platform=platform,
            )
        else:
            self._apply_platform_node_selector(pod_spec, platform)

        containers = pod_spec.get("containers", [])
        if self.runtime_class:
            pod_spec["runtimeClassName"] = self.runtime_class

        if image_spec.auth:
            secret_name = build_image_pull_secret_name(sandbox_id)
            pod_spec["imagePullSecrets"] = [{"name": secret_name}]

        apply_egress_to_spec(
            containers=containers,
            network_policy=network_policy,
            egress_image=egress_image,
            egress_auth_token=egress_auth_token,
            egress_mode=egress_mode,
        )

        if volumes:
            apply_volumes_to_pod_spec(pod_spec, volumes)

        spec: Dict[str, Any] = {
            "replicas": 1,
            "template": {
                "metadata": {
                    "labels": labels,
                    "annotations": annotations or {},
                },
                "spec": pod_spec,
            },
        }

        runtime_manifest = {
            "apiVersion": f"{self.group}/{self.version}",
            "kind": "BatchSandbox",
            "metadata": {
                "name": sandbox_id,
                "namespace": namespace,
                "labels": labels,
            },
            "spec": spec,
        }
        if annotations:
            runtime_manifest["metadata"]["annotations"] = annotations

        batchsandbox = self.template_manager.merge_with_runtime_values(runtime_manifest)
        if expires_at is None:
            batchsandbox["spec"].pop("expireTime", None)
        else:
            batchsandbox["spec"]["expireTime"] = expires_at.isoformat()
        self._merge_pod_spec_extras(batchsandbox, extra_volumes, extra_mounts)
        if platform is not None and not windows_profile:
            merged_pod_spec = batchsandbox.get("spec", {}).get("template", {}).get("spec", {})
            WorkloadProvider.ensure_platform_compatible_with_affinity(merged_pod_spec, platform)

        created = self.k8s_client.create_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            body=batchsandbox,
        )

        if image_spec.auth:
            secret = build_image_pull_secret(
                sandbox_id=sandbox_id,
                image_uri=image_spec.uri,
                auth=image_spec.auth,
                owner_uid=created["metadata"]["uid"],
                owner_api_version=f"{self.group}/{self.version}",
                owner_kind="BatchSandbox",
            )
            try:
                self.k8s_client.create_secret(namespace=namespace, body=secret)
                logger.info(f"Created imagePullSecret for sandbox {sandbox_id}")
            except Exception:
                logger.warning(f"Failed to create imagePullSecret for sandbox {sandbox_id}, rolling back BatchSandbox")
                try:
                    self.k8s_client.delete_custom_object(
                        group=self.group,
                        version=self.version,
                        namespace=namespace,
                        plural=self.plural,
                        name=sandbox_id,
                        grace_period_seconds=0,
                    )
                except Exception as del_exc:
                    logger.warning(f"Failed to rollback BatchSandbox {sandbox_id}: {del_exc}")
                raise

        return {
            "name": created["metadata"]["name"],
            "uid": created["metadata"]["uid"],
        }

    def _apply_platform_node_selector(
        self,
        pod_spec: Dict[str, Any],
        platform: Optional[PlatformSpec],
    ) -> None:
        if platform is None:
            return

        template = self.template_manager.get_base_template()
        template_spec = (
            template.get("spec", {})
            .get("template", {})
            .get("spec", {})
        )
        WorkloadProvider.apply_platform_node_selector(
            pod_spec=pod_spec,
            template_spec=template_spec if isinstance(template_spec, dict) else {},
            platform=platform,
        )

    def _create_workload_from_pool(
        self,
        batchsandbox_name: str,
        namespace: str,
        labels: Dict[str, str],
        pool_ref: str,
        expires_at: Optional[datetime],
        entrypoint: List[str],
        env: Dict[str, str],
        annotations: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        """Create a BatchSandbox by referencing an existing pool."""
        spec: Dict[str, Any] = {
            "replicas": 1,
            "poolRef": pool_ref,
        }
        needs_task_template = env or entrypoint != DEFAULT_ENTRYPOINT
        if needs_task_template:
            spec["taskTemplate"] = self._build_task_template(entrypoint, env)
        if expires_at is not None:
            spec["expireTime"] = expires_at.isoformat()
        runtime_manifest = {
            "apiVersion": f"{self.group}/{self.version}",
            "kind": "BatchSandbox",
            "metadata": {
                "name": batchsandbox_name,
                "namespace": namespace,
                "labels": labels,
            },
            "spec": spec,
        }
        if annotations:
            runtime_manifest["metadata"]["annotations"] = annotations

        created = self.k8s_client.create_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            body=runtime_manifest,
        )
        
        return {
            "name": created["metadata"]["name"],
            "uid": created["metadata"]["uid"],
        }

    def _extract_template_pod_extras(self) -> tuple[list[Dict[str, Any]], list[Dict[str, Any]]]:
        """Extract extra template volumes and mounts for runtime merge."""
        template = self.template_manager.get_base_template()
        spec = template.get("spec", {}) if isinstance(template, dict) else {}
        template_spec = spec.get("template", {}).get("spec", {})
        extra_volumes = template_spec.get("volumes", []) or []

        extra_mounts: list[Dict[str, Any]] = []
        containers = template_spec.get("containers", []) or []
        if containers:
            target = None
            for container in containers:
                if container.get("name") == "sandbox":
                    target = container
                    break
            if target is None:
                target = containers[0]
            extra_mounts = target.get("volumeMounts", []) or []

        if not isinstance(extra_volumes, list):
            extra_volumes = []
        if not isinstance(extra_mounts, list):
            extra_mounts = []
        return extra_volumes, extra_mounts

    def _merge_pod_spec_extras(
        self,
        batchsandbox: Dict[str, Any],
        extra_volumes: list[Dict[str, Any]],
        extra_mounts: list[Dict[str, Any]],
    ) -> None:
        """Merge template-provided volumes and mounts into runtime pod spec."""
        try:
            spec = batchsandbox["spec"]["template"]["spec"]
        except KeyError:
            return

        volumes = spec.get("volumes", []) or []
        if isinstance(volumes, list) and extra_volumes:
            existing = {v.get("name") for v in volumes if isinstance(v, dict)}
            for vol in extra_volumes:
                if not isinstance(vol, dict):
                    continue
                name = vol.get("name")
                if not name or name in existing:
                    continue
                volumes.append(vol)
                existing.add(name)
            spec["volumes"] = volumes

        containers = spec.get("containers", []) or []
        if not containers or not isinstance(containers, list):
            return
        main_container = containers[0]
        mounts = main_container.get("volumeMounts", []) or []
        if isinstance(mounts, list) and extra_mounts:
            existing = {m.get("name") for m in mounts if isinstance(m, dict)}
            for mnt in extra_mounts:
                if not isinstance(mnt, dict):
                    continue
                name = mnt.get("name")
                if not name or name in existing:
                    continue
                mounts.append(mnt)
                existing.add(name)
            main_container["volumeMounts"] = mounts

    def _build_task_template(
        self,
        entrypoint: List[str],
        env: Dict[str, str],
    ) -> Dict[str, Any]:
        """Build pool taskTemplate with shell-escaped bootstrap command."""
        escaped_entrypoint = ' '.join(shlex.quote(arg) for arg in entrypoint)
        user_process_cmd = f"/opt/opensandbox/bin/bootstrap.sh {escaped_entrypoint} &"
        
        wrapped_command = ["/bin/sh", "-c", user_process_cmd]

        env_list = [{"name": k, "value": v} for k, v in env.items()] if env else []

        return {
            "spec": {
                "process": {
                    "command": wrapped_command,
                    "env": env_list,
                }
            }
        }


    def get_workload(self, sandbox_id: str, namespace: str) -> Optional[Dict[str, Any]]:
        """Get BatchSandbox by sandbox ID."""
        workload = self.k8s_client.get_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            name=sandbox_id,
        )
        if workload:
            return workload

        legacy_name = self.legacy_resource_name(sandbox_id)
        if legacy_name != sandbox_id:
            return self.k8s_client.get_custom_object(
                group=self.group,
                version=self.version,
                namespace=namespace,
                plural=self.plural,
                name=legacy_name,
            )

        return None
    
    def delete_workload(self, sandbox_id: str, namespace: str) -> None:
        """Delete BatchSandbox workload."""
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise Exception(f"BatchSandbox for sandbox {sandbox_id} not found")

        self.k8s_client.delete_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            name=batchsandbox["metadata"]["name"],
            grace_period_seconds=0,
        )

    def list_workloads(self, namespace: str, label_selector: str) -> List[Dict[str, Any]]:
        """List BatchSandboxes matching label selector."""
        return self.k8s_client.list_custom_objects(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            label_selector=label_selector,
        )

    def patch_workload(self, sandbox_id: str, namespace: str, spec_patch: Dict[str, Any]) -> Dict[str, Any]:
        """Patch BatchSandbox spec (e.g., spec.pause for pause/resume)."""
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            return None
        return self.k8s_client.patch_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            name=batchsandbox["metadata"]["name"],
            body=spec_patch,
        )

    @staticmethod
    def _has_true_condition(conditions: List[Dict[str, Any]], condition_type: str) -> bool:
        for cond in conditions:
            if cond.get("type") == condition_type and cond.get("status") == "True":
                return True
        return False

    @staticmethod
    def _first_true_condition_message(conditions: List[Dict[str, Any]], condition_types: List[str]) -> Optional[str]:
        for condition_type in condition_types:
            for cond in conditions:
                if cond.get("type") == condition_type and cond.get("status") == "True":
                    message = cond.get("message")
                    if isinstance(message, str) and message.strip():
                        return message
        return None

    def _patch_pause_with_retry_bridge(self, sandbox_id: str, namespace: str, target: Optional[bool]) -> None:
        self.patch_workload(sandbox_id, namespace, {"spec": {"pause": None}})
        try:
            self.patch_workload(sandbox_id, namespace, {"spec": {"pause": target}})
            return
        except Exception as exc:
            current = self.get_workload(sandbox_id, namespace)
            current_pause = None if not current else current.get("spec", {}).get("pause")
            if current is not None and current_pause == target:
                logger.warning(
                    "BatchSandbox %s retry bridge target patch raised %s but read-back confirmed spec.pause=%s",
                    sandbox_id,
                    type(exc).__name__,
                    target,
                )
                return

            logger.warning(
                "BatchSandbox %s retry bridge target patch raised %s and current spec.pause=%s; retrying target patch once",
                sandbox_id,
                type(exc).__name__,
                current_pause,
            )
            retried = self.patch_workload(sandbox_id, namespace, {"spec": {"pause": target}})
            if retried is None:
                raise exc

    def update_expiration(self, sandbox_id: str, namespace: str, expires_at: datetime) -> None:
        """Update BatchSandbox `spec.expireTime`."""
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise Exception(f"BatchSandbox for sandbox {sandbox_id} not found")

        body = {
            "spec": {
                "expireTime": expires_at.isoformat()
            }
        }
        
        self.k8s_client.patch_custom_object(
            group=self.group,
            version=self.version,
            namespace=namespace,
            plural=self.plural,
            name=batchsandbox["metadata"]["name"],
            body=body,
        )

    def pause_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Pause a BatchSandbox by patching spec.pause=true.

        Validates that the current status.phase allows pause:
        - Succeed: allowed (fresh pause)
        - Succeed + PauseFailed=True: allowed (retry after failure, server internally patches nil->true)
        - Pausing/Resuming: not allowed (operation in progress)
        - Paused: not allowed (already paused)
        - Failed: not allowed (sandbox unavailable)
        - Failed + PauseFailed=True: not allowed (sandbox unavailable)
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        status = batchsandbox.get("status", {})
        phase = status.get("phase", "")
        conditions = status.get("conditions", [])

        pause_failed = self._has_true_condition(conditions, "PauseFailed")

        if phase == "Succeed":
            pass
        elif phase == "Pausing":
            raise ValueError(f"Cannot pause: operation in progress (phase={phase})")
        elif phase == "Resuming":
            raise ValueError(f"Cannot pause: operation in progress (phase={phase})")
        elif phase == "Paused":
            raise ValueError("Sandbox is already paused")
        elif phase == "Failed":
            if pause_failed:
                raise ValueError("Cannot pause: sandbox is not available (pause caused pod loss)")
            else:
                raise ValueError("Cannot pause: sandbox is not available")
        elif phase == "Pending":
            raise ValueError(f"Cannot pause: sandbox is being created (phase={phase})")
        else:
            raise ValueError(f"Cannot pause sandbox in phase {phase}")

        if pause_failed:
            self._patch_pause_with_retry_bridge(sandbox_id, namespace, True)
            logger.info("Patched BatchSandbox %s retry bridge spec.pause=nil->true", sandbox_id)
        else:
            self.patch_workload(sandbox_id, namespace, {"spec": {"pause": True}})
            logger.info("Patched BatchSandbox %s spec.pause=true", sandbox_id)

    def resume_sandbox(self, sandbox_id: str, namespace: str) -> None:
        """Resume a BatchSandbox by patching spec.pause=false.

        Validates that the current status.phase allows resume:
        - Paused: allowed (fresh resume)
        - Paused + ResumeFailed=True: allowed (retry after failure, server internally patches nil->false)
        - Resuming/Pausing: not allowed (operation in progress)
        - Succeed: not allowed (not paused)
        - Failed: not allowed (sandbox unavailable)
        """
        batchsandbox = self.get_workload(sandbox_id, namespace)
        if not batchsandbox:
            raise ValueError(f"Sandbox '{sandbox_id}' not found")

        status = batchsandbox.get("status", {})
        phase = status.get("phase", "")
        conditions = status.get("conditions", [])

        resume_failed = self._has_true_condition(conditions, "ResumeFailed")

        # Allow resume when Paused (or Paused with ResumeFailed for retry)
        if phase == "Paused":
            # Always allowed, even if ResumeFailed=True (retry scenario)
            pass
        elif phase == "Resuming":
            raise ValueError(f"Cannot resume: operation in progress (phase={phase})")
        elif phase == "Pausing":
            raise ValueError(f"Cannot resume: operation in progress (phase={phase})")
        elif phase == "Succeed":
            raise ValueError(f"Cannot resume sandbox in phase {phase}, expected Paused")
        elif phase == "Failed":
            if resume_failed:
                raise ValueError("Cannot resume: sandbox is not available (resume caused pod start failure)")
            else:
                raise ValueError("Cannot resume: sandbox is not available")
        elif phase == "Pending":
            raise ValueError(f"Cannot resume: sandbox is being created (phase={phase})")
        else:
            raise ValueError(f"Cannot resume sandbox in phase {phase}, expected Paused")

        if resume_failed:
            self._patch_pause_with_retry_bridge(sandbox_id, namespace, False)
            logger.info("Patched BatchSandbox %s retry bridge spec.pause=nil->false", sandbox_id)
        else:
            self.patch_workload(sandbox_id, namespace, {"spec": {"pause": False}})
            logger.info("Patched BatchSandbox %s spec.pause=false", sandbox_id)

    def get_expiration(self, workload: Dict[str, Any]) -> Optional[datetime]:
        """Parse expiration timestamp from `spec.expireTime`."""
        spec = workload.get("spec", {})
        expire_time_str = spec.get("expireTime")
        
        if not expire_time_str:
            return None
        
        try:
            return datetime.fromisoformat(expire_time_str.replace('Z', '+00:00'))
        except (ValueError, TypeError) as e:
            logger.warning(f"Invalid expireTime format: {expire_time_str}, error: {e}")
            return None

    def _parse_pod_ip(self, workload: Dict[str, Any]) -> Optional[str]:
        """Parse first pod IP from endpoints annotation."""
        annotations = workload.get("metadata", {}).get("annotations", {})
        endpoints_str = annotations.get("sandbox.opensandbox.io/endpoints")
        if not endpoints_str:
            return None
        try:
            endpoints = json.loads(endpoints_str)
            if endpoints and len(endpoints) > 0:
                return endpoints[0]
        except (json.JSONDecodeError, IndexError, TypeError):
            pass
        return None

    def _platform_unschedulable_message_from_selector(self, workload: Dict[str, Any]) -> Optional[str]:
        workload_has_platform_constraints, workload_has_non_platform_constraints = _workload_platform_constraint_scope(
            workload,
            "template",
            self.analyze_platform_constraints_in_pod_spec,
        )
        if not workload_has_platform_constraints:
            return None
        status = workload.get("status", {})
        selector = status.get("selector")
        namespace = workload.get("metadata", {}).get("namespace")
        if not selector or not namespace:
            return None
        try:
            pods = self.k8s_client.list_pods(
                namespace=namespace,
                label_selector=selector,
            )
        except Exception:
            return None

        for pod in pods:
            message = _extract_platform_unschedulable_message_from_pod(
                pod,
                workload_has_platform_constraints,
                workload_has_non_platform_constraints,
                self.is_platform_unschedulable,
            )
            if message:
                return message
        return None

    def get_status(self, workload: Dict[str, Any]) -> Dict[str, Any]:
        """Derive sandbox state from BatchSandbox status and pod readiness."""
        status = workload.get("status", {})
        creation_timestamp = workload.get("metadata", {}).get("creationTimestamp")

        # Phase is authoritative when set (Pausing/Paused/Resuming/Failed)
        phase = status.get("phase", "")
        failed_message = self._first_true_condition_message(
            status.get("conditions", []),
            ["PodFailed", "ResumeFailed", "PauseFailed"],
        )
        phase_map = {
            "Pending": ("Pending", "CREATING", "Sandbox is being created"),
            "Succeed": ("Running", "RUNNING", "Sandbox is running"),
            "Running": ("Running", "RUNNING", "Sandbox is running"),
            "Pausing": ("Pausing", "PAUSING", "Pausing sandbox"),
            "Paused": ("Paused", "PAUSED", "Sandbox is paused"),
            "Resuming": ("Resuming", "RESUMING", "Resuming sandbox"),
            "Failed": ("Failed", "FAILED", failed_message or "Operation failed"),
        }
        if phase in phase_map:
            state, reason, message = phase_map[phase]
            return {
                "state": state,
                "reason": reason,
                "message": message,
                "last_transition_at": creation_timestamp,
            }

        # Fallback: derive from pod state
        replicas = status.get("replicas", 0)
        ready = status.get("ready", 0)
        allocated = status.get("allocated", 0)
        pod_ip = self._parse_pod_ip(workload)

        if ready == 1 and pod_ip:
            state = "Running"
            reason = "POD_READY_WITH_IP"
            message = f"Pod is ready with IP ({ready}/{replicas} ready)"
        elif pod_ip:
            state = "Allocated"
            reason = "IP_ASSIGNED"
            message = f"Pod has IP assigned but not ready ({allocated}/{replicas} allocated, {ready} ready)"
        else:
            unschedulable_message = self._platform_unschedulable_message_from_selector(workload)
            if unschedulable_message:
                state = "Failed"
                reason = "POD_PLATFORM_UNSCHEDULABLE"
                message = unschedulable_message
            else:
                state = "Pending"
                reason = "POD_SCHEDULED" if allocated > 0 else "BATCHSANDBOX_PENDING"
                message = (
                    f"Pod is scheduled but waiting for IP ({allocated}/{replicas} allocated, {ready} ready)"
                    if allocated > 0
                    else "BatchSandbox is pending allocation"
                )

        return {
            "state": state,
            "reason": reason,
            "message": message,
            "last_transition_at": creation_timestamp,
        }
    
    def get_endpoint_info(self, workload: Dict[str, Any], port: int, sandbox_id: str) -> Optional[Endpoint]:
        """Resolve endpoint using gateway ingress or parsed pod IP."""
        if self.ingress_config and self.ingress_config.mode == INGRESS_MODE_GATEWAY:
            return format_ingress_endpoint(self.ingress_config, sandbox_id, port)

        pod_ip = self._parse_pod_ip(workload)
        if not pod_ip:
            return None
        return Endpoint(endpoint=f"{pod_ip}:{port}")
