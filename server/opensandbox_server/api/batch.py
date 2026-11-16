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

"""HTTP extension for native Kubernetes batch workloads."""

from functools import lru_cache
from typing import Annotated, Any

from fastapi import APIRouter, Depends, HTTPException, Path, Query, Response
from fastapi.responses import PlainTextResponse

from opensandbox_server.api.batch_schema import (
    CronJobManifest,
    JobManifest,
    RESOURCE_NAME_PATTERN,
)
from opensandbox_server.config import get_config
from opensandbox_server.services.k8s.batch_service import BatchService
from opensandbox_server.services.k8s.client import K8sClient

router = APIRouter(prefix="/kubernetes", tags=["Kubernetes Batch"])
Name = Annotated[str, Path(min_length=1, max_length=63, pattern=RESOURCE_NAME_PATTERN)]
PodName = Annotated[
    str, Path(min_length=1, max_length=253, pattern=r"^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$")
]


@lru_cache(maxsize=1)
def _batch_client() -> K8sClient:
    config = get_config().kubernetes
    assert config is not None  # get_batch_service checks the runtime before initializing.
    return K8sClient(config)


def get_batch_service() -> BatchService:
    """Enable native workload submission only for authenticated K8s deployments."""
    config = get_config()
    if (
        config.runtime.type != "kubernetes"
        or config.kubernetes is None
        or not config.kubernetes.batch_api_enabled
    ):
        raise HTTPException(
            501,
            detail={
                "code": "K8S_BATCH_NOT_SUPPORTED",
                "message": "The Kubernetes Batch API is disabled.",
            },
        )
    if not config.server.api_key and config.tenants is None:
        raise HTTPException(
            403,
            detail={
                "code": "K8S_BATCH_AUTH_REQUIRED",
                "message": "The Batch API requires API key authentication.",
            },
        )
    return BatchService(_batch_client(), config.kubernetes.namespace or "default")


Service = Annotated[BatchService, Depends(get_batch_service)]


@router.post("/jobs", status_code=201, operation_id="createKubernetesJob")
def create_job(request: JobManifest, service: Service) -> dict[str, Any]:
    """Submit a native Job; creation does not wait for completion."""
    return service.create("job", request)


@router.get("/jobs", operation_id="listKubernetesJobs")
def list_jobs(service: Service) -> dict[str, Any]:
    """List owned Jobs, including Jobs created by managed CronJobs."""
    return service.list("job")


@router.get("/jobs/{name}", operation_id="getKubernetesJob")
def get_job(name: Name, service: Service) -> dict[str, Any]:
    return service.get("job", name)


@router.get("/jobs/{name}/pods", operation_id="listKubernetesJobPods")
def get_job_pods(name: Name, service: Service) -> dict[str, Any]:
    return service.pods(name)


@router.get(
    "/jobs/{name}/pods/{pod_name}/logs",
    response_class=PlainTextResponse,
    operation_id="getKubernetesJobPodLogs",
)
def get_job_logs(
    name: Name,
    pod_name: PodName,
    service: Service,
    container: str | None = Query(None, min_length=1, max_length=63, pattern=RESOURCE_NAME_PATTERN),
    tail_lines: int = Query(100, alias="tailLines", ge=1, le=10000),
    limit_bytes: int = Query(65536, alias="limitBytes", ge=1, le=1048576),
) -> str:
    return service.logs(name, pod_name, container, tail_lines, limit_bytes)


@router.delete("/jobs/{name}", status_code=202, operation_id="deleteKubernetesJob")
def delete_job(name: Name, service: Service) -> Response:
    service.delete("job", name)
    return Response(status_code=202)


@router.post("/cronjobs", status_code=201, operation_id="createKubernetesCronJob")
def create_cronjob(request: CronJobManifest, service: Service) -> dict[str, Any]:
    return service.create("cron_job", request)


@router.get("/cronjobs", operation_id="listKubernetesCronJobs")
def list_cronjobs(service: Service) -> dict[str, Any]:
    return service.list("cron_job")


@router.get("/cronjobs/{name}", operation_id="getKubernetesCronJob")
def get_cronjob(name: Name, service: Service) -> dict[str, Any]:
    return service.get("cron_job", name)


@router.put("/cronjobs/{name}", operation_id="replaceKubernetesCronJob")
def replace_cronjob(name: Name, request: CronJobManifest, service: Service) -> dict[str, Any]:
    """Replace the desired spec, including suspend/resume and schedule changes."""
    return service.replace_cronjob(name, request)


@router.get("/cronjobs/{name}/jobs", operation_id="listKubernetesCronJobJobs")
def get_cronjob_jobs(name: Name, service: Service) -> dict[str, Any]:
    return service.list("job", cronjob=name)


@router.delete("/cronjobs/{name}", status_code=202, operation_id="deleteKubernetesCronJob")
def delete_cronjob(name: Name, service: Service) -> Response:
    service.delete("cron_job", name)
    return Response(status_code=202)
