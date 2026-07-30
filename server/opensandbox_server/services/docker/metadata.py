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

"""Per-sandbox file-backed metadata store for Docker.

Docker cannot update labels on running containers, so user metadata patches
and expiration overrides are persisted to individual JSON files under
``~/.opensandbox/metadata/``. Atomic writes (tmp + rename) guard against
truncation during crash.
"""

from __future__ import annotations

import json
import logging
from datetime import datetime
from pathlib import Path

logger = logging.getLogger(__name__)

_NON_USER_LABEL_PREFIXES = (
    "opensandbox.io/",
    "org.opencontainers.image.",
    "com.docker.",
    "desktop.docker.",
)

DEFAULT_STORE_DIR = Path.home() / ".opensandbox" / "metadata"


def _is_user_label(key: str) -> bool:
    return not any(key.startswith(p) for p in _NON_USER_LABEL_PREFIXES)


def _extract_user_labels(labels: dict) -> dict:
    """Return only user-facing labels from a raw container label dict."""
    return {k: v for k, v in labels.items() if _is_user_label(k)}


class DockerMetadataStore:
    """Per-sandbox file-backed store for user metadata and expiration overrides."""

    def __init__(self, root: Path | None = None) -> None:
        self._root = root or DEFAULT_STORE_DIR

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def get(self, sandbox_id: str, container_labels: dict) -> dict | None:
        """Return effective user metadata for a sandbox.

        If a persisted override file exists, returns that.  Otherwise falls
        back to extracting user labels from the container.
        """
        path = self._sandbox_path(sandbox_id)
        if path.exists():
            overrides = self._read_file(path)
            if overrides is not None:
                return overrides or None
        return _extract_user_labels(container_labels) or None

    def patch(
        self,
        sandbox_id: str,
        container_labels: dict,
        patch: dict,
    ) -> None:
        """Apply a JSON Merge Patch to user metadata and persist atomically.

        Callers must validate the patch (reserved keys, label format) before
        calling this method.
        """
        path = self._sandbox_path(sandbox_id)

        # Build current effective state: container user labels + persisted file
        current = _extract_user_labels(container_labels)
        if path.exists():
            overrides = self._read_file(path)
            if overrides:
                current.update(overrides)

        for key, value in patch.items():
            if value is None:
                current.pop(key, None)
            else:
                current[key] = str(value)

        self._write_file(path, current)

    def get_expiration(self, sandbox_id: str) -> str | None:
        """Return a persisted expiration override when one exists."""
        payload = self._read_file(self._expiration_path(sandbox_id))
        if not isinstance(payload, dict):
            return None
        expires_at = payload.get("expires_at")
        if isinstance(expires_at, str) and expires_at.strip():
            return expires_at
        return None

    def set_expiration(self, sandbox_id: str, expires_at: datetime) -> None:
        """Persist the latest sandbox expiration override atomically."""
        self._write_file(
            self._expiration_path(sandbox_id),
            {"expires_at": expires_at.isoformat()},
        )

    def delete(self, sandbox_id: str) -> None:
        """Remove persisted overrides for a sandbox."""
        for path in (self._sandbox_path(sandbox_id), self._expiration_path(sandbox_id)):
            try:
                path.unlink(missing_ok=True)
            except OSError as exc:
                logger.warning("Failed to delete metadata file %s: %s", path, exc)

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    def _sandbox_path(self, sandbox_id: str) -> Path:
        return self._root / f"{sandbox_id}.json"

    def _expiration_path(self, sandbox_id: str) -> Path:
        return self._root / "_expiration" / f"{sandbox_id}.json"

    @staticmethod
    def _read_file(path: Path) -> dict | None:
        try:
            if path.exists():
                return json.loads(path.read_text())
        except (json.JSONDecodeError, OSError) as exc:
            logger.warning("Failed to read metadata file %s: %s", path, exc)
        return None

    @staticmethod
    def _write_file(path: Path, data: dict) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(data))
        tmp.replace(path)
