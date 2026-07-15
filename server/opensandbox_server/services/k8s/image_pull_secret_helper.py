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
Helpers for creating Kubernetes imagePullSecrets.
"""

import base64
import json

from kubernetes.client import V1ObjectMeta, V1OwnerReference, V1Secret

from opensandbox_server.api.schema import ImageAuth

IMAGE_AUTH_SECRET_PREFIX = "opensandbox-image-auth"


def build_image_pull_secret_name(sandbox_id: str) -> str:
    """Derive a deterministic imagePullSecret name from sandbox_id."""
    return f"{IMAGE_AUTH_SECRET_PREFIX}-{sandbox_id}"


def build_image_pull_secret(
    sandbox_id: str,
    image_uri: str,
    auth: ImageAuth,
    owner_uid: str,
    owner_api_version: str,
    owner_kind: str,
) -> V1Secret:
    """
    Build a kubernetes.io/dockerconfigjson Secret for image pull auth.

    The Secret's ownerReference points to the owning CR so it is
    garbage-collected automatically when the owner is deleted.

    Args:
        sandbox_id: Sandbox identifier (used to derive Secret name)
        image_uri: Container image URI (used to determine registry hostname)
        auth: ImageAuth credentials
        owner_uid: UID of the owning CR
        owner_api_version: apiVersion of the owning CR (e.g. "sandbox.opensandbox.io/v1alpha1")
        owner_kind: Kind of the owning CR (e.g. "BatchSandbox")

    Returns:
        V1Secret ready to be created via CoreV1Api
    """
    secret_name = build_image_pull_secret_name(sandbox_id)

    # Derive registry hostname from image URI
    # e.g. "registry.example.com/ns/image:tag" -> "registry.example.com"
    # e.g. "python:3.11" -> "https://index.docker.io/v1/"
    parts = image_uri.split("/")
    if len(parts) >= 2 and ("." in parts[0] or ":" in parts[0]):
        registry = parts[0]
    else:
        registry = "https://index.docker.io/v1/"

    auth_str = base64.b64encode(
        f"{auth.username}:{auth.password}".encode()
    ).decode()
    docker_config = {
        "auths": {
            registry: {
                "username": auth.username,
                "password": auth.password,
                "auth": auth_str,
            }
        }
    }
    docker_config_b64 = base64.b64encode(
        json.dumps(docker_config).encode()
    ).decode()

    return V1Secret(
        api_version="v1",
        kind="Secret",
        metadata=V1ObjectMeta(
            name=secret_name,
            owner_references=[
                V1OwnerReference(
                    api_version=owner_api_version,
                    kind=owner_kind,
                    name=sandbox_id,
                    uid=owner_uid,
                    controller=False,
                )
            ],
        ),
        type="kubernetes.io/dockerconfigjson",
        data={".dockerconfigjson": docker_config_b64},
    )
