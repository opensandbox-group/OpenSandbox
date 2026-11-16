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

"""Native Job/CronJob management; Kubernetes owns their lifecycle."""

from contextlib import contextmanager
from typing import Any, Iterator, Literal, cast

from fastapi import HTTPException
from kubernetes.client import ApiException
from urllib3.exceptions import HTTPError

from opensandbox_server.api.batch_schema import (
    BATCH_MANAGED_LABEL,
    CronJobManifest,
    JobManifest,
)
from opensandbox_server.services.constants import SANDBOX_TENANT_LABEL
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.tenants.context import get_current_tenant

BatchKind = Literal["job", "cron_job"]


def _error(status: int, message: str) -> HTTPException:
    return HTTPException(
        status_code=status, detail={"code": "K8S_BATCH_API_ERROR", "message": message}
    )


@contextmanager
def _kubernetes_errors() -> Iterator[None]:
    """Preserve actionable API status codes without exposing submitted specs."""
    try:
        yield
    except ApiException as exc:
        status = exc.status if exc.status in (400, 403, 404, 409, 422, 429) else 503
        # API-server bodies can echo environment values or other credentials.
        raise _error(status, f"Kubernetes batch request failed: {exc.reason or status}") from exc
    except HTTPError as exc:
        raise _error(503, "Kubernetes API is unavailable.") from exc


class BatchService:
    """Manage only batch resources owned by the authenticated tenant."""

    def __init__(self, client: K8sClient, namespace: str) -> None:
        self.client = client
        self.serialize = client.get_batch_v1_api().api_client.sanitize_for_serialization
        tenant = get_current_tenant()
        self.namespace = tenant.namespace if tenant else namespace
        self.labels = {
            BATCH_MANAGED_LABEL: "server",
            SANDBOX_TENANT_LABEL: tenant.name if tenant else "",
        }

    def _manifest(self, request: JobManifest | CronJobManifest) -> dict[str, Any]:
        if request.metadata.namespace not in (None, self.namespace):
            raise _error(403, "The namespace must match the authenticated workload namespace.")
        body = request.model_dump(by_alias=True, exclude_none=True)
        body["metadata"]["namespace"] = self.namespace
        body["metadata"]["labels"].update(self.labels)
        if request.kind == "CronJob":
            metadata = body["spec"]["jobTemplate"].setdefault("metadata", {})
            metadata.setdefault("labels", {}).update(self.labels)
        return body

    def _owned(self, resource: Any) -> bool:
        labels = resource.metadata.labels or {}
        return all(labels.get(key) == value for key, value in self.labels.items())

    @staticmethod
    def _child(resource: Any, kind: str, uid: str) -> bool:
        return any(
            owner.controller and owner.kind == kind and owner.uid == uid
            for owner in resource.metadata.owner_references or []
        )

    def _get(self, kind: BatchKind, name: str) -> Any:
        resource = self.client.batch_request(kind, "read", self.namespace, name=name)
        if not self._owned(resource):
            raise _error(404, "Batch resource not found.")
        return resource

    def create(self, kind: BatchKind, request: JobManifest | CronJobManifest) -> dict[str, Any]:
        """Create a resource with server-owned namespace and ownership labels."""
        body = self._manifest(request)
        with _kubernetes_errors():
            result = self.client.batch_request(kind, "create", self.namespace, body=body)
        return cast(dict[str, Any], self.serialize(result))

    def get(self, kind: BatchKind, name: str) -> dict[str, Any]:
        """Read an owned resource, including native Kubernetes status."""
        with _kubernetes_errors():
            return cast(dict[str, Any], self.serialize(self._get(kind, name)))

    def list(self, kind: BatchKind, cronjob: str | None = None) -> dict[str, Any]:
        """List owned resources, optionally restricting Jobs to a CronJob UID."""
        selector = ",".join(f"{key}={value}" for key, value in self.labels.items())
        with _kubernetes_errors():
            parent = self._get("cron_job", cronjob) if cronjob else None
            result = self.client.batch_request(
                kind, "list", self.namespace, label_selector=selector
            )
            items = [
                item
                for item in result.items
                if self._owned(item)
                and (parent is None or self._child(item, "CronJob", parent.metadata.uid))
            ]
        return {"items": self.serialize(items)}

    def replace_cronjob(self, name: str, request: CronJobManifest) -> dict[str, Any]:
        """Replace desired CronJob state; existing Jobs keep their original spec."""
        if request.metadata.name != name:
            raise _error(400, "metadata.name must match the request path.")
        body = self._manifest(request)
        with _kubernetes_errors():
            current = self._get("cron_job", name)
            # Fence replacement against changes after the ownership check.
            body["metadata"]["resourceVersion"] = current.metadata.resource_version
            body["metadata"]["uid"] = current.metadata.uid
            result = self.client.batch_request(
                "cron_job", "replace", self.namespace, name=name, body=body
            )
        return cast(dict[str, Any], self.serialize(result))

    def delete(self, kind: BatchKind, name: str) -> None:
        """Request cascading deletion, guarded by the UID that was authorized."""
        with _kubernetes_errors():
            resource = self._get(kind, name)
            self.client.batch_request(
                kind,
                "delete",
                self.namespace,
                name=name,
                body={
                    "propagationPolicy": "Background",
                    "preconditions": {"uid": resource.metadata.uid},
                },
            )

    def pods(self, name: str) -> dict[str, Any]:
        """Return Pod diagnostics only for Pods owned by this Job UID."""
        with _kubernetes_errors():
            job = self._get("job", name)
            pods = self.client.list_pods(
                self.namespace,
                label_selector=f"batch.kubernetes.io/controller-uid={job.metadata.uid}",
            )
        return {
            "items": self.serialize(
                [pod for pod in pods if self._child(pod, "Job", job.metadata.uid)]
            )
        }

    def logs(
        self,
        name: str,
        pod_name: str,
        container: str | None,
        tail_lines: int,
        limit_bytes: int,
    ) -> str:
        """Read bounded logs from a Pod verified against the current Job UID."""
        with _kubernetes_errors():
            job = self._get("job", name)
            pod = self.client.read_pod(self.namespace, pod_name)
            if pod is None or not self._child(pod, "Job", job.metadata.uid):
                raise _error(404, "Job Pod not found.")
            return self.client.read_pod_logs(
                self.namespace, pod_name, container, tail_lines, limit_bytes
            )
