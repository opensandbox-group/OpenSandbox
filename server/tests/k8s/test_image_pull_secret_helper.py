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

import base64
import json

from opensandbox_server.api.schema import ImageAuth
from opensandbox_server.services.k8s.image_pull_secret_helper import (
    IMAGE_AUTH_SECRET_PREFIX,
    build_image_pull_secret,
    build_image_pull_secret_name,
)


class TestBuildImagePullSecretName:

    def test_returns_deterministic_name(self):
        assert build_image_pull_secret_name("abc123") == f"{IMAGE_AUTH_SECRET_PREFIX}-abc123"

    def test_different_ids_produce_different_names(self):
        assert build_image_pull_secret_name("id-1") != build_image_pull_secret_name("id-2")


class TestBuildImagePullSecret:

    def _auth(self, username="user", password="pass") -> ImageAuth:
        return ImageAuth(username=username, password=password)

    def _decode_docker_config(self, secret) -> dict:
        raw = base64.b64decode(secret.data[".dockerconfigjson"])
        return json.loads(raw)

    def test_secret_metadata(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="registry.example.com/ns/img:tag",
            auth=self._auth(),
            owner_uid="uid-1",
            owner_api_version="sandbox.opensandbox.io/v1alpha1",
            owner_kind="BatchSandbox",
        )
        assert secret.metadata.name == f"{IMAGE_AUTH_SECRET_PREFIX}-sid"
        assert secret.type == "kubernetes.io/dockerconfigjson"
        assert secret.api_version == "v1"
        assert secret.kind == "Secret"

    def test_owner_reference(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="registry.example.com/img:tag",
            auth=self._auth(),
            owner_uid="uid-abc",
            owner_api_version="sandbox.opensandbox.io/v1alpha1",
            owner_kind="BatchSandbox",
        )
        refs = secret.metadata.owner_references
        assert len(refs) == 1
        ref = refs[0]
        assert ref.uid == "uid-abc"
        assert ref.api_version == "sandbox.opensandbox.io/v1alpha1"
        assert ref.kind == "BatchSandbox"
        assert ref.name == "sid"
        assert ref.controller is False

    def test_private_registry_extracted_from_image_uri(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="registry.example.com/ns/img:tag",
            auth=self._auth("u", "p"),
            owner_uid="uid",
            owner_api_version="sandbox.opensandbox.io/v1alpha1",
            owner_kind="BatchSandbox",
        )
        config = self._decode_docker_config(secret)
        assert config["auths"].get("registry.example.com") is not None

    def test_docker_hub_image_uses_default_registry(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="python:3.11",
            auth=self._auth("u", "p"),
            owner_uid="uid",
            owner_api_version="sandbox.opensandbox.io/v1alpha1",
            owner_kind="BatchSandbox",
        )
        config = self._decode_docker_config(secret)
        assert "https://index.docker.io/v1/" in config["auths"]

    def test_auth_credentials_encoded_correctly(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="registry.example.com/img:tag",
            auth=self._auth("myuser", "mypass"),
            owner_uid="uid",
            owner_api_version="sandbox.opensandbox.io/v1alpha1",
            owner_kind="BatchSandbox",
        )
        config = self._decode_docker_config(secret)
        registry_config = config["auths"]["registry.example.com"]
        assert registry_config["username"] == "myuser"
        assert registry_config["password"] == "mypass"
        expected_auth = base64.b64encode(b"myuser:mypass").decode()
        assert registry_config["auth"] == expected_auth

    def test_image_with_port_uses_host_port_as_registry(self):
        secret = build_image_pull_secret(
            sandbox_id="sid",
            image_uri="localhost:5000/myimage:latest",
            auth=self._auth(),
            owner_uid="uid",
            owner_api_version="v1alpha1",
            owner_kind="BatchSandbox",
        )
        config = self._decode_docker_config(secret)
        assert "localhost:5000" in config["auths"]
