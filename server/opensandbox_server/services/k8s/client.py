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
Kubernetes client wrapper that provides a unified interface for all K8s resource
operations. All API access goes through this class.
"""

import logging
import threading
from functools import partial
from typing import Any, Dict, List, Optional, Tuple

from kubernetes import client, config
from kubernetes.client import ApiException, CoreV1Api, CustomObjectsApi, NodeV1Api

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.k8s.informer import WorkloadInformer
from opensandbox_server.services.k8s.label_selector import matches, parse_selector
from opensandbox_server.services.k8s.rate_limiter import TokenBucketRateLimiter

logger = logging.getLogger(__name__)

_InformerKey = Tuple[str, str, str, str]  # (group, version, plural, namespace)


class K8sClient:
    """
    Unified Kubernetes API client.

    Encapsulates all cluster resource operations (CustomObject, Secret, Pod,
    RuntimeClass). Callers never hold raw API handles directly.
    """

    def __init__(self, k8s_config: KubernetesRuntimeConfig):
        self.config = k8s_config
        self._load_config()
        self._core_v1_api: Optional[CoreV1Api] = None
        self._custom_objects_api: Optional[CustomObjectsApi] = None
        self._node_v1_api: Optional[NodeV1Api] = None
        self._informers: Dict[_InformerKey, WorkloadInformer] = {}
        self._informers_lock = threading.Lock()
        self._read_limiter: Optional[TokenBucketRateLimiter] = (
            TokenBucketRateLimiter(qps=k8s_config.read_qps, burst=k8s_config.read_burst)
            if k8s_config.read_qps > 0
            else None
        )
        self._write_limiter: Optional[TokenBucketRateLimiter] = (
            TokenBucketRateLimiter(qps=k8s_config.write_qps, burst=k8s_config.write_burst)
            if k8s_config.write_qps > 0
            else None
        )

    def _load_config(self) -> None:
        """Load kubeconfig from file path or in-cluster service account."""
        try:
            if self.config.kubeconfig_path:
                config.load_kube_config(config_file=self.config.kubeconfig_path)
            else:
                config.load_incluster_config()
        except Exception as e:
            raise Exception(f"Failed to load Kubernetes configuration: {e}") from e

    def get_core_v1_api(self) -> CoreV1Api:
        if self._core_v1_api is None:
            self._core_v1_api = client.CoreV1Api()
        return self._core_v1_api

    def get_custom_objects_api(self) -> CustomObjectsApi:
        if self._custom_objects_api is None:
            self._custom_objects_api = client.CustomObjectsApi()
        return self._custom_objects_api

    def get_node_v1_api(self) -> NodeV1Api:
        if self._node_v1_api is None:
            self._node_v1_api = client.NodeV1Api()
        return self._node_v1_api


    def _lookup_informer(self, group: str, version: str, plural: str, namespace: str) -> Optional[WorkloadInformer]:
        """Return an existing informer without starting one. Used by write paths
        to invalidate cache entries; never auto-create on writes since list paths
        own the lazy-start contract."""
        if not self.config.informer_enabled:
            return None
        key: _InformerKey = (group, version, plural, namespace)
        with self._informers_lock:
            return self._informers.get(key)

    def _get_informer(self, group: str, version: str, plural: str, namespace: str) -> Optional[WorkloadInformer]:
        """Return the informer for this resource+namespace, starting it lazily."""
        if not self.config.informer_enabled:
            return None

        key: _InformerKey = (group, version, plural, namespace)
        with self._informers_lock:
            informer = self._informers.get(key)
            if informer is None:
                list_fn = partial(
                    self.get_custom_objects_api().list_namespaced_custom_object,
                    group=group,
                    version=version,
                    namespace=namespace,
                    plural=plural,
                )
                informer = WorkloadInformer(
                    list_fn=list_fn,
                    resync_period_seconds=self.config.informer_resync_seconds,
                    watch_timeout_seconds=self.config.informer_watch_timeout_seconds,
                    thread_name=f"workload-informer-{plural}-{namespace}",
                )
                self._informers[key] = informer
                try:
                    informer.start()
                except Exception as exc:  # pragma: no cover - defensive
                    logger.warning(f"Failed to start informer for {plural}/{namespace}: {exc}")
                    self._informers.pop(key, None)
                    return None
        return informer


    def create_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        body: Dict[str, Any],
    ) -> Dict[str, Any]:
        """Create a namespaced custom resource."""
        if self._write_limiter:
            self._write_limiter.acquire()
        obj = self.get_custom_objects_api().create_namespaced_custom_object(
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            body=body,
        )
        informer = self._lookup_informer(group, version, plural, namespace)
        if informer:
            informer.update_cache(obj)
        return obj

    def get_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        name: str,
    ) -> Optional[Dict[str, Any]]:
        """Get a namespaced custom resource by name.

        Tries the informer cache first when available and synced.
        Returns None on 404.
        """
        informer = self._get_informer(group, version, plural, namespace)
        if informer and informer.has_synced:
            cached = informer.get(name)
            if cached is not None:
                return cached

        if self._read_limiter:
            self._read_limiter.acquire()
        try:
            obj = self.get_custom_objects_api().get_namespaced_custom_object(
                group=group,
                version=version,
                namespace=namespace,
                plural=plural,
                name=name,
            )
            if informer:
                informer.update_cache(obj)
            return obj
        except ApiException as e:
            if e.status == 404:
                return None
            raise

    def list_custom_objects(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        label_selector: str = "",
    ) -> List[Dict[str, Any]]:
        """List namespaced custom resources, returning the items list.

        Tries the informer cache first when available, synced, and the label
        selector falls within the supported in-memory grammar. Falls back to
        a direct API call (with rate limiting) otherwise.
        """
        informer = self._get_informer(group, version, plural, namespace)
        if informer and informer.has_synced:
            terms = parse_selector(label_selector)
            if terms is not None:
                cached = informer.list()
                if not terms:
                    return cached
                return [
                    obj
                    for obj in cached
                    if matches(obj.get("metadata", {}).get("labels") or {}, terms)
                ]

        if self._read_limiter:
            self._read_limiter.acquire()
        try:
            resp = self.get_custom_objects_api().list_namespaced_custom_object(
                group=group,
                version=version,
                namespace=namespace,
                plural=plural,
                label_selector=label_selector,
            )
            return resp.get("items", [])
        except ApiException as e:
            if e.status == 404:
                return []
            raise

    def delete_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        name: str,
        grace_period_seconds: int = 0,
    ) -> None:
        """Delete a namespaced custom resource."""
        if self._write_limiter:
            self._write_limiter.acquire()
        self.get_custom_objects_api().delete_namespaced_custom_object(
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
            grace_period_seconds=grace_period_seconds,
        )
        informer = self._lookup_informer(group, version, plural, namespace)
        if informer:
            informer.delete_from_cache(name)

    def patch_custom_object(
        self,
        group: str,
        version: str,
        namespace: str,
        plural: str,
        name: str,
        body: Dict[str, Any],
    ) -> Dict[str, Any]:
        """Patch a namespaced custom resource."""
        if self._write_limiter:
            self._write_limiter.acquire()
        obj = self.get_custom_objects_api().patch_namespaced_custom_object(
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
            body=body,
        )
        informer = self._lookup_informer(group, version, plural, namespace)
        if informer:
            informer.update_cache(obj)
        return obj

    # ------------------------------------------------------------------
    # PersistentVolumeClaim operations
    # ------------------------------------------------------------------

    def get_pvc(
        self,
        namespace: str,
        name: str,
    ) -> Optional[Any]:
        """Read a PersistentVolumeClaim by name. Returns None on 404."""
        if self._read_limiter:
            self._read_limiter.acquire()
        try:
            return self.get_core_v1_api().read_namespaced_persistent_volume_claim(
                name=name,
                namespace=namespace,
            )
        except ApiException as e:
            if e.status == 404:
                return None
            raise

    def create_pvc(
        self,
        namespace: str,
        body: Any,
    ) -> Any:
        """Create a PersistentVolumeClaim."""
        if self._write_limiter:
            self._write_limiter.acquire()
        return self.get_core_v1_api().create_namespaced_persistent_volume_claim(
            namespace=namespace,
            body=body,
        )

    def list_pvcs(
        self,
        namespace: str,
        label_selector: str = "",
    ) -> List[Any]:
        """List PersistentVolumeClaims in a namespace, returning the items list."""
        if self._read_limiter:
            self._read_limiter.acquire()
        result = self.get_core_v1_api().list_namespaced_persistent_volume_claim(
            namespace=namespace,
            label_selector=label_selector,
        )
        return list(getattr(result, "items", []) or [])

    def delete_pvc(
        self,
        namespace: str,
        name: str,
    ) -> None:
        """Delete a PersistentVolumeClaim by name. 404 is swallowed."""
        if self._write_limiter:
            self._write_limiter.acquire()
        try:
            self.get_core_v1_api().delete_namespaced_persistent_volume_claim(
                name=name,
                namespace=namespace,
            )
        except ApiException as e:
            if e.status == 404:
                return
            raise

    def patch_pvc(
        self,
        namespace: str,
        name: str,
        body: Any,
    ) -> Any:
        """Patch a PersistentVolumeClaim using strategic merge semantics.

        The pinned kubernetes-client picks ``application/json-patch+json`` by
        default (first entry in the generated content-type list) which would
        reject our merge-shaped body. Force strategic-merge so list fields
        like ``ownerReferences`` merge by key.
        """
        if self._write_limiter:
            self._write_limiter.acquire()
        return self.get_core_v1_api().patch_namespaced_persistent_volume_claim(
            name=name,
            namespace=namespace,
            body=body,
            _content_type="application/strategic-merge-patch+json",
        )

    # ------------------------------------------------------------------
    # Secret operations
    # ------------------------------------------------------------------

    def create_secret(self, namespace: str, body: Any) -> Any:
        """Create a namespaced Secret."""
        if self._write_limiter:
            self._write_limiter.acquire()
        return self.get_core_v1_api().create_namespaced_secret(
            namespace=namespace,
            body=body,
        )


    def list_pods(
        self,
        namespace: str,
        label_selector: str = "",
    ) -> List[Any]:
        """List pods in a namespace, returning the items list."""
        if self._read_limiter:
            self._read_limiter.acquire()
        resp = self.get_core_v1_api().list_namespaced_pod(
            namespace=namespace,
            label_selector=label_selector,
        )
        return resp.items

    def read_runtime_class(self, name: str) -> Any:
        """Read a RuntimeClass from the cluster."""
        if self._read_limiter:
            self._read_limiter.acquire()
        return self.get_node_v1_api().read_runtime_class(name)
