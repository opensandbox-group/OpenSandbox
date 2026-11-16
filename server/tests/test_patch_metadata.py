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

"""Unit tests for patch metadata merge logic and service implementations."""

import pytest
from fastapi import HTTPException
from unittest.mock import MagicMock

from opensandbox_server.services.constants import SandboxErrorCodes


class TestIsSystemLabel:
    """Test system label detection (opensandbox.io/ prefix)."""

    def test_user_label_not_system(self):
        from opensandbox_server.services.sandbox_service import SandboxService

        assert not SandboxService._is_system_label("team")
        assert not SandboxService._is_system_label("app.kubernetes.io/name")
        assert not SandboxService._is_system_label("example.com/label")

    def test_opensandbox_prefixed_is_system(self):
        from opensandbox_server.services.sandbox_service import SandboxService

        assert SandboxService._is_system_label("opensandbox.io/id")
        assert SandboxService._is_system_label("opensandbox.io/expires-at")
        assert SandboxService._is_system_label("opensandbox.io/custom-key")


class TestApplyMetadataPatch:
    """Test shared _apply_metadata_patch logic."""

    @staticmethod
    def _merge(labels: dict, patch: dict) -> dict:
        from opensandbox_server.services.sandbox_service import SandboxService
        return SandboxService._apply_metadata_patch(labels, patch)

    def test_preserves_system_labels(self):
        labels = {
            "opensandbox.io/id": "sbx-001",
            "opensandbox.io/expires-at": "2026-01-01T00:00:00Z",
            "team": "infra",
        }
        result = self._merge(labels, {"project": "new"})
        assert result["opensandbox.io/id"] == "sbx-001"
        assert result["opensandbox.io/expires-at"] == "2026-01-01T00:00:00Z"

    def test_adds_user_metadata(self):
        labels = {"opensandbox.io/id": "sbx-001"}
        result = self._merge(labels, {"team": "platform"})
        assert result["team"] == "platform"

    def test_deletes_user_metadata(self):
        labels = {
            "opensandbox.io/id": "sbx-001",
            "team": "infra",
            "project": "old",
        }
        result = self._merge(labels, {"team": None})
        assert "team" not in result
        assert "project" in result
        assert result["opensandbox.io/id"] == "sbx-001"

    def test_replaces_user_metadata(self):
        labels = {
            "opensandbox.io/id": "sbx-001",
            "team": "old-team",
        }
        result = self._merge(labels, {"team": "new-team"})
        assert result["team"] == "new-team"

    def test_empty_patch_no_change(self):
        labels = {
            "opensandbox.io/id": "sbx-001",
            "team": "infra",
        }
        result = self._merge(labels, {})
        assert result == labels

    def test_reserved_key_rejected_even_with_null_value(self):
        from fastapi import HTTPException

        labels = {
            "opensandbox.io/id": "sbx-001",
            "team": "infra",
        }
        with pytest.raises(HTTPException) as exc:
            self._merge(labels, {"opensandbox.io/id": None})
        assert exc.value.status_code == 400
        assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
        assert "opensandbox.io/id" in exc.value.detail["message"]

    def test_reserved_key_rejected_with_non_null_value(self):
        from fastapi import HTTPException

        labels = {"opensandbox.io/id": "sbx-001"}
        with pytest.raises(HTTPException) as exc:
            self._merge(labels, {"opensandbox.io/custom": "value"})
        assert exc.value.status_code == 400
        assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
        assert "opensandbox.io/custom" in exc.value.detail["message"]


class TestPatchLabelsMethod:
    """Test WorkloadProvider.patch_labels."""

    @staticmethod
    def _make_provider(**overrides):
        from opensandbox_server.services.k8s.workload_provider import WorkloadProvider

        class StubProvider(WorkloadProvider):
            group = "test.group"
            version = "v1"
            plural = "tests"

            def create_workload(self, *args, **kwargs): pass
            def get_workload(self, *args, **kwargs): pass
            def delete_workload(self, *args, **kwargs): pass
            def list_workloads(self, *args, **kwargs): pass
            def update_expiration(self, *args, **kwargs): pass
            def get_expiration(self, *args, **kwargs): pass
            def get_status(self, *args, **kwargs): pass
            def get_endpoint_info(self, *args, **kwargs): pass

        for attr, val in overrides.items():
            setattr(StubProvider, attr, val)
        return StubProvider()

    def test_patches_labels_via_k8s_client(self):
        mock_client = MagicMock()
        provider = self._make_provider()
        provider.k8s_client = mock_client

        provider.patch_labels(name="sandbox-sbx-001", namespace="default", labels={"team": "infra"})

        mock_client.patch_custom_object.assert_called_once()
        call_kwargs = mock_client.patch_custom_object.call_args
        assert call_kwargs[1]["group"] == "test.group"
        assert call_kwargs[1]["name"] == "sandbox-sbx-001"
        assert call_kwargs[1]["body"] == {"metadata": {"labels": {"team": "infra"}}}


class TestPatchMetadataValidation:
    def test_reserved_prefix_rejected(self):
        from opensandbox_server.services.validators import ensure_metadata_labels

        with pytest.raises(HTTPException) as exc:
            ensure_metadata_labels({"opensandbox.io/custom": "value"})
        assert exc.value.status_code == 400
        assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL

    def test_valid_metadata_accepted(self):
        from opensandbox_server.services.validators import ensure_metadata_labels

        ensure_metadata_labels({"team": "platform", "version": "2.0"})

    def test_null_values_not_validated(self):
        """Null values (deletions) do not need validation — they are removed before validate."""
        from opensandbox_server.services.sandbox_service import SandboxService

        labels = SandboxService._apply_metadata_patch(
            {"team": "platform"},
            {"Invalid Key!": None},
        )
        assert labels == {"team": "platform"}


class TestDockerPatchMetadataReservedKey:
    """Docker rejects reserved keys with the canonical metadata error code."""

    def test_reserved_key_rejected_with_canonical_code(self, monkeypatch, tmp_path):
        from unittest.mock import patch

        from opensandbox_server.config import AppConfig, RuntimeConfig
        from opensandbox_server.services.docker import DockerSandboxService

        monkeypatch.setattr(
            "opensandbox_server.services.docker.metadata.DEFAULT_STORE_DIR",
            tmp_path / "metadata",
        )
        config = AppConfig(
            runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"),
        )
        with patch("opensandbox_server.services.docker.docker_service.docker") as mock_docker:
            mock_client = MagicMock()
            mock_client.containers.list.return_value = []
            mock_docker.from_env.return_value = mock_client
            service = DockerSandboxService(config=config)

        container = MagicMock()
        container.attrs = {"Config": {"Labels": {"opensandbox.io/id": "sbx-001"}}}
        monkeypatch.setattr(service, "_get_container_by_sandbox_id", lambda _id: container)

        with pytest.raises(HTTPException) as exc:
            service.patch_sandbox_metadata("sbx-001", {"opensandbox.io/expires-at": "never"})
        assert exc.value.status_code == 400
        assert exc.value.detail["code"] == SandboxErrorCodes.INVALID_METADATA_LABEL
