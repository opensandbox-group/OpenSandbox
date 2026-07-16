#!/usr/bin/env python3

from __future__ import annotations

import argparse
import importlib
import os
from pathlib import Path
from typing import Any

yaml: Any = importlib.import_module("yaml")

# Upstream's kind-jwt overlay is the closest JWT-authenticated topology to this
# POC, but it contains Kind-only images/flags and Kubernetes APIs absent on the
# target AKS cluster. This helper performs only deterministic YAML transforms;
# setup_poc.sh owns cluster access, credentials, ordering, and readiness waits.

DEFAULT_IMAGES = {
    "ko://github.com/agent-substrate/substrate/cmd/ateapi": (
        "ryanclaw.azurecr.io/opensandbox-substrate-poc/"
        "ateapi-752889f8b0bcdbee32172ac9fe056025@"
        "sha256:c33dbf444740429228b34b06d2622d25844d77aada1f59412cee6402c3e653dc"
    ),
    "ko://github.com/agent-substrate/substrate/cmd/atecontroller": (
        "ryanclaw.azurecr.io/opensandbox-substrate-poc/"
        "atecontroller-86b02a85c6bff82898e0930a8b0286d1@"
        "sha256:bb9299b27def0dbf08d1c7f104d3abe578c71db1ef479dba849558f03fe741ca"
    ),
    "ko://github.com/agent-substrate/substrate/cmd/atenet": (
        "ryanclaw.azurecr.io/opensandbox-substrate-poc/"
        "atenet-2559047aaba794c6723c99da32f86409@"
        "sha256:f4ac238705af3cc9d107598c2960e3b945ff230e356ac838a2420b9a26d41523"
    ),
    "ko://github.com/agent-substrate/substrate/cmd/atelet": (
        "ryanclaw.azurecr.io/opensandbox-substrate-poc/"
        "atelet-89dbecdd4e8d5cd4d125a2de341f399c@"
        "sha256:c96637b94261d0988d73573443499613c12a57b770d25e360b1d1c41f7cadac4"
    ),
}

DEFAULT_ATEOM_IMAGE = (
    "ryanclaw.azurecr.io/opensandbox-substrate-poc@"
    "sha256:8c76eeceaaa9515b891febcb8db28c540686ad06da6e0f698d42a4142f2c87f0"
)
DEFAULT_EXECD_IMAGE = (
    "ryanclaw.azurecr.io/opensandbox-substrate-poc/execd@"
    "sha256:0948f25b00840813c4036672c7811b3b9671a60e5dc20460b2524083d31d7d1b"
)
DEFAULT_SERVER_IMAGE = (
    "ryanclaw.azurecr.io/opensandbox-substrate-poc/opensandbox-server@"
    "sha256:9185bdbdac904d62ba6168378fe6254dc6b7e897a1c628c5c06bf459208a9210"
)

DROP_NAMES = {
    "dns",
    "atenet-dns",
    "prometheus",
    "ate-prometheus",
    "podcert-ate-dev-signer",
    "podcertificate-controller-is-a-coordinator",
}
DROP_NAMESPACES = {"podcertificate-controller-system", "otel-system"}
REMOVE_VOLUME_NAMES = {
    "session-id-jwt-pool",
    "session-id-ca-pool",
    "workerpool-ca-certs",
}
REMOVE_ARG_PREFIXES = (
    "--session-id-jwt-pool=",
    "--session-id-ca-pool=",
    "--workerpool-ca-certs=",
    "--otlp-collector-address=",
    "--localhost-registry-replacement=",
)
POC_LABELS = {
    "app.kubernetes.io/part-of": "opensandbox-substrate-poc",
    "opensandbox.io/poc": "agent-substrate",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--system-output", type=Path, required=True)
    parser.add_argument("--workload-template", type=Path, required=True)
    parser.add_argument("--workload-output", type=Path, required=True)
    parser.add_argument("--server-template", type=Path, required=True)
    parser.add_argument("--server-output", type=Path, required=True)
    return parser.parse_args()


def static_volume(name: str) -> dict[str, Any] | None:
    # Replace upstream Pod Certificate projections with the static POC Secret.
    if name == "servicedns":
        return {
            "name": name,
            "secret": {
                "secretName": "substrate-static-pki",
                "items": [
                    {
                        "key": "credential-bundle.pem",
                        "path": "credential-bundle.pem",
                    }
                ],
            },
        }
    if name == "servicedns-ca":
        return {
            "name": name,
            "secret": {
                "secretName": "substrate-static-pki",
                "items": [{"key": "ca.crt", "path": "trust-bundle.pem"}],
            },
        }
    return None


def image_map() -> dict[str, str]:
    return {
        source: os.environ.get(
            {
                "ateapi": "ATEAPI_IMAGE",
                "atecontroller": "ATECONTROLLER_IMAGE",
                "atenet": "ATENET_IMAGE",
                "atelet": "ATELET_IMAGE",
            }[source.rsplit("/", 1)[-1]],
            default,
        )
        for source, default in DEFAULT_IMAGES.items()
    }


def patch_pod_spec(spec: dict[str, Any], images: dict[str, str]) -> None:
    # Remove unused auth/observability mounts and resolve pinned component images.
    volumes = []
    for volume in spec.get("volumes", []) or []:
        name = volume.get("name")
        if name in REMOVE_VOLUME_NAMES:
            continue
        volumes.append(static_volume(name) or volume)
    if "volumes" in spec:
        spec["volumes"] = volumes

    containers = (spec.get("containers", []) or []) + (
        spec.get("initContainers", []) or []
    )
    for container in containers:
        image = container.get("image")
        if image in images:
            container["image"] = images[image]
        if "volumeMounts" in container:
            container["volumeMounts"] = [
                mount
                for mount in container["volumeMounts"]
                if mount.get("name") not in REMOVE_VOLUME_NAMES
            ]
        if "args" in container:
            container["args"] = [
                arg
                for arg in container["args"]
                if not any(str(arg).startswith(prefix) for prefix in REMOVE_ARG_PREFIXES)
            ]
        if "env" in container:
            container["env"] = [
                value
                for value in container["env"]
                if value.get("name") != "OTEL_EXPORTER_OTLP_ENDPOINT"
            ]


def should_drop(document: dict[str, Any]) -> bool:
    metadata = document.get("metadata") or {}
    name = metadata.get("name", "")
    namespace = metadata.get("namespace")
    return (
        namespace in DROP_NAMESPACES
        or (document.get("kind") == "Namespace" and name in DROP_NAMESPACES)
        or name in DROP_NAMES
        or "podcertificate" in name
    )


def prepare_system(source: Path) -> list[dict[str, Any]]:
    # Keep only the control, storage, router, and node-runtime resources needed
    # by this POC. Cluster APIs and SandboxConfig are applied from upstream files.
    images = image_map()
    result = []
    for document in yaml.safe_load_all(source.read_text(encoding="utf-8")):
        if not isinstance(document, dict) or should_drop(document):
            continue

        metadata = document.setdefault("metadata", {})
        metadata.setdefault("labels", {}).update(POC_LABELS)
        kind = document.get("kind")
        spec = document.get("spec") or {}
        if kind in {"Deployment", "DaemonSet", "StatefulSet", "Job"}:
            patch_pod_spec(spec.get("template", {}).get("spec", {}), images)
        if kind == "PersistentVolumeClaim":
            spec["storageClassName"] = os.environ.get(
                "STORAGE_CLASS", "managed-csi"
            )
        if kind == "StatefulSet":
            for claim in spec.get("volumeClaimTemplates", []) or []:
                claim.setdefault("spec", {})["storageClassName"] = os.environ.get(
                    "STORAGE_CLASS", "managed-csi"
                )
        result.append(document)

    rendered = yaml.safe_dump_all(result, sort_keys=False)
    # Fail closed if upstream changes reintroduce an unsupported API or an
    # unresolved development image into the generated deployment.
    prohibited = (
        "podCertificate:",
        "clusterTrustBundle:",
        "ko://",
        "kind-registry:5000",
        "namespace: podcertificate-controller-system",
        "namespace: otel-system",
    )
    for value in prohibited:
        if value in rendered:
            raise ValueError(f"AKS manifest still contains prohibited value: {value}")
    if len(result) != 30:
        raise ValueError(f"expected 30 Agent Substrate resources, got {len(result)}")
    return result


def load_documents(path: Path) -> list[dict[str, Any]]:
    return [
        document
        for document in yaml.safe_load_all(path.read_text(encoding="utf-8"))
        if isinstance(document, dict)
    ]


def prepare_workload(path: Path) -> list[dict[str, Any]]:
    # Inject immutable Worker and execd images into administrator-owned CRs.
    documents = load_documents(path)
    for document in documents:
        if document["kind"] == "WorkerPool":
            document["spec"]["ateomImage"] = os.environ.get(
                "ATEOM_IMAGE", DEFAULT_ATEOM_IMAGE
            )
        elif document["kind"] == "ActorTemplate":
            document["spec"]["containers"][0]["image"] = os.environ.get(
                "EXECD_IMAGE", DEFAULT_EXECD_IMAGE
            )
    return documents


def prepare_server(path: Path) -> list[dict[str, Any]]:
    # Keep server configuration declarative while allowing a rebuilt image pin.
    documents = load_documents(path)
    for document in documents:
        if document["kind"] == "Deployment" and document["metadata"]["name"] == (
            "opensandbox-server"
        ):
            document["spec"]["template"]["spec"]["containers"][0]["image"] = (
                os.environ.get("OPENSANDBOX_SERVER_IMAGE", DEFAULT_SERVER_IMAGE)
            )
    return documents


def write_documents(path: Path, documents: list[dict[str, Any]]) -> None:
    path.write_text(
        yaml.safe_dump_all(documents, sort_keys=False),
        encoding="utf-8",
    )


def main() -> None:
    args = parse_args()
    write_documents(args.system_output, prepare_system(args.source))
    write_documents(args.workload_output, prepare_workload(args.workload_template))
    write_documents(args.server_output, prepare_server(args.server_template))


if __name__ == "__main__":
    main()