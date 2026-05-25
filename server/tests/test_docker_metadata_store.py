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

"""Unit tests for DockerMetadataStore (per-sandbox file-backed)."""

from pathlib import Path

from opensandbox_server.services.docker.metadata import (
    DockerMetadataStore,
    _extract_user_labels,
    _is_user_label,
)


class TestIsUserLabel:
    def test_user_labels_return_true(self):
        assert _is_user_label("team")
        assert _is_user_label("app.kubernetes.io/name")
        assert _is_user_label("example.com/label")

    def test_opensandbox_prefix_returns_false(self):
        assert not _is_user_label("opensandbox.io/id")
        assert not _is_user_label("opensandbox.io/expires-at")

    def test_oci_labels_return_false(self):
        assert not _is_user_label("org.opencontainers.image.created")
        assert not _is_user_label("org.opencontainers.image.version")

    def test_docker_labels_return_false(self):
        assert not _is_user_label("com.docker.some-label")
        assert not _is_user_label("desktop.docker.other")


class TestExtractUserLabels:
    def test_filters_non_user_labels(self):
        labels = {
            "team": "infra",
            "opensandbox.io/id": "sbx-001",
            "org.opencontainers.image.created": "2026-01-01",
            "com.docker.foo": "bar",
        }
        result = _extract_user_labels(labels)
        assert result == {"team": "infra"}

    def test_empty_returns_empty(self):
        assert _extract_user_labels({}) == {}


class TestDockerMetadataStore:

    @staticmethod
    def _make_store(tmp_path: Path):
        return DockerMetadataStore(root=tmp_path / "metadata")

    # -- get (no overrides) --------------------------------------------------

    def test_get_no_overrides_returns_user_labels(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {
            "team": "infra",
            "opensandbox.io/id": "sbx-001",
            "org.opencontainers.image.created": "2026-01-01",
        }
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra"}

    def test_get_no_user_labels_returns_none(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        result = store.get("sbx-001", {"opensandbox.io/id": "sbx-001"})
        assert result is None

    # -- patch ---------------------------------------------------------------

    def test_patch_adds_keys(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra"}
        store.patch("sbx-001", labels, {"project": "new"})
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra", "project": "new"}

    def test_patch_deletes_keys(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra", "project": "old"}
        store.patch("sbx-001", labels, {"project": None})
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra"}

    def test_patch_replaces_keys(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "old-team"}
        store.patch("sbx-001", labels, {"team": "new-team"})
        result = store.get("sbx-001", labels)
        assert result == {"team": "new-team"}

    def test_patch_persists_across_instances(self, tmp_path: Path):
        root = tmp_path / "metadata"
        store1 = DockerMetadataStore(root=root)
        store1.patch("sbx-001", {"hello": "world"}, {"team": "platform"})

        store2 = DockerMetadataStore(root=root)
        result = store2.get("sbx-001", {"hello": "world"})
        assert result == {"hello": "world", "team": "platform"}

    def test_patch_empty_noop(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra"}
        store.patch("sbx-001", labels, {})
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra"}

    def test_patch_from_empty_labels(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels: dict = {}
        store.patch("sbx-001", labels, {"new": "value"})
        result = store.get("sbx-001", labels)
        assert result == {"new": "value"}

    def test_patch_delete_nonexistent_silent(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra"}
        store.patch("sbx-001", labels, {"nonexistent": None})
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra"}

    def test_patch_mixed_add_and_delete(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra", "env": "staging", "old": "remove"}
        store.patch("sbx-001", labels, {"team": "platform", "env": None, "project": "new"})
        result = store.get("sbx-001", labels)
        assert result == {"team": "platform", "old": "remove", "project": "new"}

    def test_patch_ignores_system_labels(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {
            "hello": "world",
            "opensandbox.io/id": "sbx-001",
            "org.opencontainers.image.version": "26.04",
        }
        store.patch("sbx-001", labels, {"team": "platform"})
        result = store.get("sbx-001", labels)
        assert result == {"hello": "world", "team": "platform"}

    # -- delete --------------------------------------------------------------

    def test_delete_removes_file(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        labels = {"team": "infra"}
        store.patch("sbx-001", labels, {"project": "new"})

        f = tmp_path / "metadata" / "sbx-001.json"
        assert f.exists()

        store.delete("sbx-001")
        assert not f.exists()
        # Falls back to container labels
        result = store.get("sbx-001", labels)
        assert result == {"team": "infra"}

    def test_delete_nonexistent_silent(self, tmp_path: Path):
        store = self._make_store(tmp_path)
        store.delete("nonexistent")  # Should not raise

    # -- crash recovery ------------------------------------------------------

    def test_tmp_file_not_read_on_reload(self, tmp_path: Path):
        """Tmp files written during atomic save must not be picked up by get()."""
        root = tmp_path / "metadata"
        store1 = DockerMetadataStore(root=root)
        store1.patch("sbx-001", {"hello": "world"}, {"team": "platform"})

        # Simulate a leftover .tmp file
        tmp_file = root / "sbx-001.tmp"
        tmp_file.write_text('{"bad": "data"}')

        store2 = DockerMetadataStore(root=root)
        result = store2.get("sbx-001", {"hello": "world"})
        assert result == {"hello": "world", "team": "platform"}
