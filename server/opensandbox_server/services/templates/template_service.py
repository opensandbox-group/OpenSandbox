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

"""FastSandboxTemplateService: fsb template management.

The server store is the source of truth for the public catalog; the
fast-sandbox ``SandboxTemplate`` CRD is the execution projection. Reads
lazily sync the CRD build status into the row, so the catalog converges
without a background task.
"""

from __future__ import annotations

import logging
import math
import re
import uuid
from datetime import datetime, timezone
from typing import Optional

from fastapi import HTTPException, status
from kubernetes.client import ApiException

from opensandbox_server.api.schema import (
    CreateFsbTemplateRequest,
    FsbTemplate,
    FsbTemplateStatus,
)
from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig
from opensandbox_server.repositories.templates import (
    FastSandboxTemplateRepository,
    create_fsb_template_repository,
)
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.templates.template_models import (
    FastSandboxTemplateListQuery,
    FastSandboxTemplatePhase,
    FastSandboxTemplateRecord,
)
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.validators import ensure_metadata_labels

logger = logging.getLogger(__name__)

GROUP = "sandbox.fast.io"
VERSION = "v1alpha2"
PLURAL = "sandboxtemplates"

# Server-side build inputs: not client fields. The kernel must
# exist in the sandboxtemplate-builder image under this name (builder
# default); execd comes from runtime.execd_image.
TEMPLATE_KERNEL = "vmlinux.bin"
# OSEP-0018 on fsb: the guest init sources the baked envs and launches
# bootstrap.sh, so a baked EXECD_INIT=1 makes bootstrap.sh exec into
# `execd --init` — execd runs as a subreaper under the guest init (no PID 1
# signal shield). Injected at CRD projection from runtime.execd_run_as_init;
# the server value wins over a client-provided env of the same name.
EXECD_INIT_ENV = "EXECD_INIT"
DEFAULT_VCPU = "1"
DEFAULT_MEMORY = "512Mi"
# Guest rootfs logical size when the client omits resourceLimits.disk. The
# artifact set is uploaded and P2P-pulled at this size (plain s3 cp is not
# sparse-aware), so the fallback stays small instead of the 30Gi CRD default.
DEFAULT_ROOTFS_SIZE = "2Gi"
DEFAULT_ENTRYPOINT = ["tail", "-f", "/dev/null"]

# Template resourceLimits only accept these keys; anything else would be
# silently dropped by the CRD mapping.
TEMPLATE_RESOURCE_KEYS = frozenset({"cpu", "memory", "disk"})

# Same shape the SandboxTemplate builder enforces at build time; validating
# here turns a would-be failed build into a 400 at template creation.
_VALID_ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")

# Artifact encodings accepted for SandboxTemplate builds. overlaybd is the
# fast-sandbox OCI path: output.publish must be a repository-only oci://
# target (the builder generates the per-build <uuid>_rootfs/_snapfiles tags
# itself). native stays on the object-store path.
_TEMPLATE_FORMATS = ("native", "overlaybd")

_TEMPLATE_NOT_FOUND = {
    "code": SandboxErrorCodes.FSB_TEMPLATE_NOT_FOUND,
    "message": "Template not found.",
}


def _publish_validation_error(format: str, publish: str) -> Optional[str]:
    """Reject format/publish pairs the builder would reject, as a 400 here.

    overlaybd: ``oci://host/repository`` — port allowed, no userinfo
    (credentials live in publishSecretRef), no tag/digest (the system
    generates the build tags). native: ``s3://bucket/prefix``.
    """
    if format == "overlaybd":
        if not publish.startswith("oci://"):
            return (
                "overlaybd templates publish to an OCI registry: "
                "publish must be 'oci://registry/repository' "
                "(S3-compatible stores are not supported for overlaybd)."
            )
        remainder = publish[len("oci://"):]
        host, _, repository = remainder.partition("/")
        if not host or not repository:
            return (
                "publish must be 'oci://registry/repository' with a "
                "non-empty registry host and repository path."
            )
        if "@" in host:
            return (
                "publish must not embed credentials in the registry host "
                "(userinfo like user:pass@host is rejected; publish "
                "credentials come from publishSecretRef)."
            )
        if "@" in repository or ":" in repository:
            return (
                "publish must be repository-only: tags and digests are "
                "rejected because the system generates an immutable "
                "per-build tag pair."
            )
        if repository.endswith("/"):
            return (
                "publish must be 'oci://registry/repository' without a "
                "trailing slash."
            )
        path_component = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
        if (
            not re.fullmatch(r"(?:[a-zA-Z0-9.-]+|\[[0-9a-fA-F:]+\])(?::[0-9]+)?", host)
            or not re.fullmatch(rf"{path_component}(?:/{path_component})*", repository)
        ):
            return "publish must be a valid oci://registry/repository without a tag or digest."
        return None
    if format == "native":
        if not publish.startswith("s3://"):
            return (
                "native templates publish to an S3-compatible store: "
                "publish must be 's3://bucket/prefix'."
            )
        if not publish[len("s3://"):].partition("/")[0]:
            return (
                "publish must be 's3://bucket/prefix' with a non-empty "
                "bucket name."
            )
        return None
    return (
        f"Unsupported template format {format!r}; "
        f"supported formats: {', '.join(_TEMPLATE_FORMATS)}."
    )


class FastSandboxTemplateService:
    """Template catalog + SandboxTemplate CRD orchestration for fsb."""

    def __init__(
        self,
        config: AppConfig,
        repository: Optional[FastSandboxTemplateRepository] = None,
        k8s_client: Optional[K8sClient] = None,
    ):
        self._config = config
        self._execd_image = config.runtime.execd_image
        self._execd_run_as_init = bool(config.runtime.execd_run_as_init)
        self._repository = repository
        self._k8s_client = k8s_client
        self._k8s_config = config.kubernetes or KubernetesRuntimeConfig()
        self._watched_namespaces: set[str] = set()

    def close(self) -> None:
        if self._k8s_client is not None:
            self._k8s_client.stop_informers()
        self._watched_namespaces.clear()

    # -- watch reactor -----------------------------------------------------------

    def start_background_sync(self) -> None:
        """React to SandboxTemplate CR changes by updating the DB directly.

        One LIST/WATCH informer per namespace that owns template rows (plus
        the configured default); every watch event and reconciling LIST item
        converges the matching row immediately, with no polling loop.
        """
        namespaces = {self._k8s_config.namespace or "default"}
        try:
            namespaces.update(self._repo().namespaces())
        except Exception as exc:  # noqa: BLE001 - catalog may be empty/unavailable
            logger.warning(f"Template catalog scan failed while starting watches: {exc}")
        for namespace in sorted(namespaces):
            self._ensure_namespace_watch(namespace)

    def _ensure_namespace_watch(self, namespace: str) -> None:
        if namespace in self._watched_namespaces:
            return
        informer = self._kubernetes().watch_custom_objects(
            GROUP, VERSION, namespace, PLURAL, self._on_template_event(namespace)
        )
        if informer is None:
            logger.warning(
                f"Template status watch failed to start for namespace {namespace!r}; "
                "rows converge on reads only"
            )
            return
        self._watched_namespaces.add(namespace)

    def _on_template_event(self, namespace: str):
        def handler(event_type: str, obj: dict) -> None:
            name = obj.get("metadata", {}).get("name", "")
            if not name:
                return
            record = self._repo().get_by_crd_name(namespace, name)
            if record is None:
                return  # not a server-managed template
            if event_type == "DELETED":
                if record.phase in (FastSandboxTemplatePhase.PENDING, FastSandboxTemplatePhase.BUILDING):
                    self._update_phase(
                        record, FastSandboxTemplatePhase.FAILED, None, "SandboxTemplate CRD is gone."
                    )
                return
            crd_status = obj.get("status") or {}
            phase = _map_phase(crd_status.get("phase"))
            if phase is None:
                return
            manifest_ref = crd_status.get("manifestRef") or None
            message = _failure_message(crd_status)
            if (
                phase is not record.phase
                or manifest_ref != record.manifest_ref
                or (phase is FastSandboxTemplatePhase.FAILED and message != record.message)
            ):
                self._update_phase(record, phase, manifest_ref, message)

        return handler

    # -- wiring --------------------------------------------------------------

    def _repo(self) -> FastSandboxTemplateRepository:
        if self._repository is None:
            self._repository = create_fsb_template_repository(self._config)
        return self._repository

    def _kubernetes(self) -> K8sClient:
        if self._k8s_client is None:
            self._k8s_client = K8sClient(self._k8s_config)
        return self._k8s_client

    def _resolve_namespace(self) -> str:
        """Tenant-scoped fast-sandbox namespace (same rule as sandboxes)."""
        from opensandbox_server.tenants.context import get_current_tenant

        tenant = get_current_tenant()
        if tenant is not None:
            return tenant.namespace
        return self._k8s_config.namespace or "default"

    # -- lifecycle -------------------------------------------------------------

    def create_template(self, request: CreateFsbTemplateRequest) -> FastSandboxTemplateRecord:
        ensure_metadata_labels(request.metadata)
        if request.resource_limits is not None:
            unsupported = set(request.resource_limits.root) - TEMPLATE_RESOURCE_KEYS
            if unsupported:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            f"Unsupported resourceLimits keys for templates: "
                            f"{', '.join(sorted(unsupported))}; supported: cpu, memory, disk."
                        ),
                    },
                )
        if request.env:
            invalid_env_names = [
                name for name in request.env if not _VALID_ENV_NAME.fullmatch(name)
            ]
            if invalid_env_names:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": (
                            f"Invalid env names for templates: "
                            f"{', '.join(sorted(invalid_env_names))}; names must "
                            f"match [A-Za-z_][A-Za-z0-9_]*."
                        ),
                    },
                )
        publish = request.publish.strip()
        publish_error = _publish_validation_error(request.format, publish)
        if publish_error:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": publish_error,
                },
            )
        if (
            request.format == "overlaybd"
            and not self._k8s_config.template_oci_publish_secret
        ):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Server is not configured for overlaybd templates: "
                        "set [kubernetes].template_oci_publish_secret to a "
                        "kubernetes.io/dockerconfigjson Secret in the "
                        "template namespace."
                    ),
                },
            )
        namespace = self._resolve_namespace()
        template_id = f"tpl-{uuid.uuid4()}"
        now = datetime.now(timezone.utc)
        spec = {
            "image": request.image.strip(),
            "resourceLimits": (
                request.resource_limits.root if request.resource_limits is not None else None
            ),
            "entrypoint": list(request.entrypoint) if request.entrypoint else None,
            "env": dict(request.env) if request.env else None,
            "readiness": (
                request.readiness.model_dump(exclude_none=True, by_alias=True)
                if request.readiness is not None
                else None
            ),
            "publish": publish,
            "format": request.format,
        }
        record = FastSandboxTemplateRecord(
            template_id=template_id,
            namespace=namespace,
            crd_name=template_id,
            spec=spec,
            metadata=dict(request.metadata or {}),
            phase=FastSandboxTemplatePhase.PENDING,
            created_at=now,
            updated_at=now,
        )
        self._repo().create(record)
        try:
            self._kubernetes().create_custom_object(
                GROUP, VERSION, namespace, PLURAL, self._build_crd(record)
            )
        except ApiException as exc:
            self._repo().delete(template_id, namespace)
            if exc.status == 409:
                raise HTTPException(
                    status_code=status.HTTP_409_CONFLICT,
                    detail={
                        "code": SandboxErrorCodes.FSB_TEMPLATE_CONFLICT,
                        "message": "A template build with the same name already exists.",
                    },
                ) from exc
            if exc.status in (400, 422):
                # e.g. an invalid resourceLimits value rejected by the CRD
                # schema; surface the API server message instead of a 503.
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": f"SandboxTemplate rejected: {exc.reason}",
                    },
                ) from exc
            logger.warning(f"SandboxTemplate CRD create failed: {exc}")
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": "SandboxTemplate CRDs are unavailable.",
                },
            ) from exc
        except Exception as exc:
            # Transport failures (connection refused, DNS, timeout) are not
            # ApiException; the catalog row must roll back all the same.
            self._repo().delete(template_id, namespace)
            logger.warning(f"SandboxTemplate CRD create failed: {exc}")
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.FSB_API_ERROR,
                    "message": "SandboxTemplate CRDs are unavailable.",
                },
            ) from exc
        # The namespace may be new (fresh tenant): start watching it now so
        # the row converges without waiting for a read.
        self._ensure_namespace_watch(namespace)
        return record

    def get_template(self, template_id: str) -> FastSandboxTemplateRecord:
        namespace = self._resolve_namespace()
        record = self._repo().get(template_id, namespace)
        if record is None:
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=_TEMPLATE_NOT_FOUND)
        return record

    def list_templates(
        self,
        *,
        metadata: Optional[dict[str, str]] = None,
        page: int = 1,
        page_size: int = 20,
    ) -> tuple[list[FastSandboxTemplateRecord], int]:
        namespace = self._resolve_namespace()
        result = self._repo().list(
            FastSandboxTemplateListQuery(
                namespace=namespace,
                metadata=metadata,
                page=page,
                page_size=page_size,
            )
        )
        return result.items, result.total_items

    def delete_template(self, template_id: str) -> None:
        namespace = self._resolve_namespace()
        record = self._repo().get(template_id, namespace)
        if record is None:
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=_TEMPLATE_NOT_FOUND)
        try:
            self._kubernetes().delete_custom_object(
                GROUP, VERSION, namespace, PLURAL, record.crd_name
            )
        except ApiException as exc:
            if exc.status != 404:
                logger.warning(f"SandboxTemplate CRD delete failed: {exc}")
                raise HTTPException(
                    status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                    detail={
                        "code": SandboxErrorCodes.FSB_API_ERROR,
                        "message": "SandboxTemplate CRDs are unavailable.",
                    },
                ) from exc
        self._repo().delete(template_id, namespace)

    def resolve_template_artifact(self, template_id: str) -> tuple[str, list[str]]:
        """Resolve a Succeeded template to its runnable artifact + entrypoint.

        native → the template id (the builder's content-addressed index key);
        overlaybd → the CRD's immutable OCI manifest_ref. Unknown,
        non-Succeeded, or manifest-less templates all yield 404, so no
        existence information leaks. Reads the DB row only; the watch
        reactor converges build phase.
        """
        namespace = self._resolve_namespace()
        record = self._repo().get(template_id, namespace)
        if record is None:
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=_TEMPLATE_NOT_FOUND)
        if record.phase in (FastSandboxTemplatePhase.PENDING, FastSandboxTemplatePhase.BUILDING):
            # Re-arm the watch so a non-terminal row cannot get stuck.
            self._ensure_namespace_watch(namespace)
        if record.phase is not FastSandboxTemplatePhase.SUCCEEDED:
            raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail=_TEMPLATE_NOT_FOUND)
        entrypoint = list(record.spec.get("entrypoint") or DEFAULT_ENTRYPOINT)
        if str(record.spec.get("format") or "overlaybd") != "overlaybd":
            return record.template_id, entrypoint
        manifest_ref = record.manifest_ref or ""
        if not re.fullmatch(re.escape(record.publish) + r"@sha256:[0-9a-f]{64}", manifest_ref):
            # Transient (status not landed) or a legacy overlaybd+S3 row
            # (rejected upstream). Both 404; the legacy message is
            # actionable for the owner.
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.FSB_TEMPLATE_NOT_FOUND,
                    "message": (
                        "Template artifact is not runnable: overlaybd "
                        "templates require an immutable OCI manifestRef in the publish repository from a build "
                        "against the current fast-sandbox contract; rebuild "
                        "the template."
                    ),
                },
            )
        return manifest_ref, entrypoint

    # -- CRD mapping -----------------------------------------------------------

    def _publish_secret_name(self, format: str) -> str:
        """Publish secret for a template format: overlaybd builds publish to
        an OCI registry (dockerconfigjson Secret), native builds to the
        object store (S3 credentials)."""
        if format == "overlaybd":
            return self._k8s_config.template_oci_publish_secret or ""
        return self._k8s_config.template_s3_publish_secret

    def _build_crd(self, record: FastSandboxTemplateRecord) -> dict:
        limits = record.spec.get("resourceLimits") or {}
        vcpu = str(limits.get("cpu") or DEFAULT_VCPU)
        memory = str(limits.get("memory") or DEFAULT_MEMORY)
        # resourceLimits.disk (parallel to cpu/memory) is the logical size of
        # the guest rootfs; artifacts are stored and pulled at this size.
        rootfs_size = str(limits.get("disk") or DEFAULT_ROOTFS_SIZE)
        fmt = str(record.spec.get("format") or "overlaybd")
        spec: dict = {
            "image": record.source_image,
            # Native artifact identity: the builder publishes the image
            # index under BOTH this key (exact, immutable per template) and
            # the legacy sha256(source-image) key (last-writer-wins, kept for
            # warmImages and older clients). Sandbox creates carry the
            # template key, so two templates of the same source image resolve
            # to their own artifact sets and caches. OCI creates instead use
            # the immutable manifestRef recorded after publication.
            "indexKey": record.template_id,
            "entrypoint": list(record.spec.get("entrypoint") or DEFAULT_ENTRYPOINT),
            "execd": self._execd_image,
            "kernel": TEMPLATE_KERNEL,
            "machine": {"vcpu": vcpu, "memory": memory},
            "output": {
                "rootfsSize": rootfs_size,
                "format": fmt,
                "publish": record.publish,
                "publishSecretRef": {"name": self._publish_secret_name(fmt)},
            },
        }
        env = record.spec.get("env") or {}
        if self._execd_run_as_init:
            # Copy first: the stored row keeps the client env untouched, and
            # the server value wins over a client-provided EXECD_INIT.
            env = {**env, EXECD_INIT_ENV: "1"}
        if env:
            spec["envs"] = [{"name": name, "value": value} for name, value in env.items()]
        # OSEP-0022 CA wiring: bake the pinned MITM CA Secret's certificate
        # entry (never the private key) into every server-created template
        # when configured. The Secret must exist in the template's
        # namespace (ops-runbook distribution); a missed copy fails the
        # build observably.
        if self._k8s_config.template_mitm_ca_secret:
            mitm_certificate: dict = {
                "secretRef": {"name": self._k8s_config.template_mitm_ca_secret}
            }
            if self._k8s_config.template_mitm_ca_secret_key:
                mitm_certificate["key"] = self._k8s_config.template_mitm_ca_secret_key
            spec["egressMitmCertificate"] = mitm_certificate
        # The CRD requires the readiness object itself (structural
        # defaulting fills warmupSeconds=60); always emit it, empty when the
        # request carried no readiness gate.
        readiness = record.spec.get("readiness") or {}
        readiness_spec: dict = {}
        if readiness.get("probe"):
            readiness_spec["probe"] = readiness["probe"]
        if readiness.get("warmupSeconds") is not None:
            readiness_spec["warmupSeconds"] = readiness["warmupSeconds"]
        spec["readiness"] = readiness_spec
        metadata: dict = {"name": record.crd_name, "namespace": record.namespace}
        if record.metadata:
            # fast-sandbox persists metadata as labels; values were validated
            # as Kubernetes label values at POST.
            metadata["labels"] = dict(record.metadata)
        return {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "SandboxTemplate",
            "metadata": metadata,
            "spec": spec,
        }

    # -- status sync -------------------------------------------------------------
    #
    # The watch reactor is the only CR->DB phase path; reads trust the DB.

    def _update_phase(
        self,
        record: FastSandboxTemplateRecord,
        phase: FastSandboxTemplatePhase,
        manifest_ref: Optional[str],
        message: Optional[str],
    ) -> None:
        record.phase = phase
        record.manifest_ref = manifest_ref
        record.message = message
        record.updated_at = datetime.now(timezone.utc)
        try:
            self._repo().update_status(
                record.template_id,
                record.namespace,
                phase=phase,
                manifest_ref=manifest_ref,
                message=message,
            )
        except Exception as exc:  # noqa: BLE001 - reads converge on the next request
            logger.warning(f"Template status persist failed: {exc}")


def _map_phase(value) -> Optional[FastSandboxTemplatePhase]:
    try:
        return FastSandboxTemplatePhase(str(value))
    except ValueError:
        return None


def _failure_message(crd_status: dict) -> Optional[str]:
    for condition in crd_status.get("conditions") or []:
        if condition.get("type") == "Failed" or str(crd_status.get("phase")) == "Failed":
            return condition.get("message") or condition.get("reason")
    return None


def template_to_response(record: FastSandboxTemplateRecord) -> FsbTemplate:
    limits = record.spec.get("resourceLimits")
    readiness = record.spec.get("readiness")
    return FsbTemplate(
        templateId=record.template_id,
        image=record.source_image,
        resourceLimits=limits,  # type: ignore[arg-type]
        entrypoint=record.spec.get("entrypoint"),
        env=record.spec.get("env"),
        metadata=record.metadata or None,
        readiness=readiness,  # type: ignore[arg-type]
        publish=record.publish,
        format=record.spec.get("format", "overlaybd"),
        status=FsbTemplateStatus(
            phase=record.phase.value,
            manifestRef=record.manifest_ref,
            message=record.message,
        ),
        createdAt=record.created_at or datetime.now(timezone.utc),
        updatedAt=record.updated_at or datetime.now(timezone.utc),
    )


def total_pages(total_items: int, page_size: int) -> int:
    return math.ceil(total_items / page_size) if page_size > 0 else 0


__all__ = ["FastSandboxTemplateService", "template_to_response", "total_pages"]
