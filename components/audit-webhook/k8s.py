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

"""Kubernetes access for sandbox liveness checks.

Two views of the live sandboxes in a namespace, via the kubeconfig:

- the BatchSandbox resources (``batchsandboxes.sandbox.opensandbox.io``)
  - the resource name is the sandbox id, used to tell which sandbox ids
  still exist (deleted-sync) and to discover never-accessed sandboxes;
- the sandbox pods - each pod's ownerReference points at its
  BatchSandbox, mapping each sandbox id to the IP of the node the pod
  runs on. (The pods carry no ``opensandbox.io/id`` label in this
  cluster, so the owner reference is the source of truth.)
"""

import logging
from datetime import datetime, timezone

from kubernetes import client, config

logger = logging.getLogger(__name__)

_BATCH_SANDBOX_GROUP = "sandbox.opensandbox.io"
_BATCH_SANDBOX_VERSION = "v1alpha1"
_BATCH_SANDBOX_PLURAL = "batchsandboxes"

# The ownerReference kind linking a sandbox pod to its BatchSandbox (the
# reference's name is the sandbox id); see list_sandbox_pod_nodes.
_BATCH_SANDBOX_KIND = "BatchSandbox"


class K8sError(Exception):
    """Raised when the Kubernetes API cannot be reached or queried."""


class K8sClient:
    """Lists BatchSandbox resource names, configured from a kubeconfig.

    The kubeconfig is loaded once, lazily: ``kubeconfig`` is an explicit
    file path; when empty, the default kubeconfig is used, falling back
    to in-cluster credentials.
    """

    def __init__(self, kubeconfig: str = ""):
        self.kubeconfig = kubeconfig
        # Cached API handles; named differently from the accessor methods
        # so the instance attributes never shadow them.
        self._custom_api: client.CustomObjectsApi | None = None
        self._core_api: client.CoreV1Api | None = None

    def _load_config(self) -> None:
        """Load the kubeconfig once; raise ``K8sError`` on failure."""
        try:
            if self.kubeconfig:
                config.load_kube_config(config_file=self.kubeconfig)
            else:
                try:
                    config.load_kube_config()
                except config.ConfigException:
                    config.load_incluster_config()
        except Exception as exc:
            raise K8sError(f"failed to load kubeconfig: {exc}") from exc

    def _custom_objects_api(self) -> client.CustomObjectsApi:
        if self._custom_api is None:
            self._load_config()
            self._custom_api = client.CustomObjectsApi()
        return self._custom_api

    def _core_v1_api(self) -> client.CoreV1Api:
        if self._core_api is None:
            self._load_config()
            self._core_api = client.CoreV1Api()
        return self._core_api

    def list_batch_sandboxes(self, namespace: str) -> list[dict]:
        """Return the BatchSandbox resources in ``namespace``.

        Each item is ``{"name": <resource name = sandbox id>, "created_at":
        <metadata.creationTimestamp parsed as an aware datetime, or None>}``.
        """
        try:
            response = self._custom_objects_api().list_namespaced_custom_object(
                group=_BATCH_SANDBOX_GROUP,
                version=_BATCH_SANDBOX_VERSION,
                namespace=namespace,
                plural=_BATCH_SANDBOX_PLURAL,
            )
        except K8sError:
            raise
        except Exception as exc:
            raise K8sError(
                f"failed to list batchsandboxes in namespace {namespace!r}: {exc}"
            ) from exc
        sandboxes = [
            {
                "name": metadata.get("name"),
                "created_at": _parse_rfc3339(metadata.get("creationTimestamp")),
            }
            for metadata in (item.get("metadata", {}) for item in response.get("items", []))
        ]
        logger.info("listed %d batchsandboxes in namespace %s", len(sandboxes), namespace)
        return sandboxes

    def list_sandbox_pod_nodes(self, namespace: str) -> dict[str, str]:
        """Return the node IP per sandbox id for the pods in ``namespace``.

        A sandbox pod is identified by an ownerReference to its
        BatchSandbox (the reference's name is the sandbox id); the node
        IP is the pod's ``status.hostIP``. Pods without a BatchSandbox
        owner or without a scheduled node yet (no host IP) are skipped;
        when a sandbox owns several pods (replicas), the first one wins.
        """
        try:
            response = self._core_v1_api().list_namespaced_pod(namespace=namespace)
        except K8sError:
            raise
        except Exception as exc:
            raise K8sError(
                f"failed to list sandbox pods in namespace {namespace!r}: {exc}"
            ) from exc
        nodes: dict[str, str] = {}
        for pod in response.items:
            metadata = pod.metadata
            host_ip = pod.status.host_ip if pod.status else None
            if metadata is None or not host_ip:
                continue
            owner = next(
                (
                    ref
                    for ref in (metadata.owner_references or [])
                    if ref.kind == _BATCH_SANDBOX_KIND
                ),
                None,
            )
            if owner is not None:
                nodes.setdefault(owner.name, host_ip)
        logger.info(
            "listed %d sandbox pods on %d nodes in namespace %s",
            len(nodes), len(set(nodes.values())), namespace,
        )
        return nodes


def _parse_rfc3339(value: str | None) -> datetime | None:
    """Parse an RFC 3339 timestamp (e.g. ``2026-08-31T01:23:45Z``).

    Naive values are assumed to be UTC; unparseable input logs a warning
    and yields ``None`` rather than failing the sync.
    """
    if not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        logger.warning("unparseable creationTimestamp %r", value)
        return None
    return parsed if parsed.tzinfo is not None else parsed.replace(tzinfo=timezone.utc)
