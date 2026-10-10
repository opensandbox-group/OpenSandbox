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

from __future__ import annotations

import json
import re
from typing import Any

from fastapi import HTTPException, status

from app.config import Settings

_LOG_OBJECT_RE = re.compile(r"/sandbox(?:\.\d+)?\.log$")
_MARKER_RE = re.compile(r"/sandbox\.finalized\.(\d+)\.json$")


def _oss_configured(settings: Settings) -> bool:
    return bool(
        settings.bff_nodeagent_oss_endpoint
        and settings.bff_nodeagent_oss_bucket
        and settings.bff_nodeagent_oss_access_key_id
        and settings.bff_nodeagent_oss_access_key_secret
    )


def _read_archive_sync(
    settings: Settings,
    namespace: str,
    sandbox_id: str,
    *,
    max_bytes: int,
) -> dict[str, Any]:
    import oss2

    prefix_parts = [
        settings.bff_nodeagent_oss_key_prefix.strip("/"),
        settings.bff_nodeagent_cluster_id,
        namespace,
        sandbox_id,
    ]
    prefix = "/".join(p for p in prefix_parts if p) + "/"

    auth = oss2.Auth(settings.bff_nodeagent_oss_access_key_id, settings.bff_nodeagent_oss_access_key_secret)
    bucket = oss2.Bucket(auth, settings.bff_nodeagent_oss_endpoint, settings.bff_nodeagent_oss_bucket)

    log_objects: list[tuple[str, Any]] = []
    marker_objects: list[tuple[str, int, Any]] = []

    for obj in oss2.ObjectIterator(bucket, prefix=prefix):
        key = obj.key
        if _LOG_OBJECT_RE.search(key):
            log_objects.append((key, obj.last_modified))
        marker_match = _MARKER_RE.search(key)
        if marker_match:
            marker_objects.append((key, int(marker_match.group(1)), obj.last_modified))

    if not log_objects:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={
                "code": "ARCHIVE_NOT_FOUND",
                "message": f"No node-agent log objects under prefix {prefix!r}",
            },
        )

    log_objects.sort(key=lambda x: x[1], reverse=True)
    chosen_key = log_objects[0][0]

    marker_status: str | None = None
    marker_warnings: list[str] = []
    if marker_objects:
        marker_objects.sort(key=lambda x: (x[1], x[2]), reverse=True)
        marker_key = marker_objects[0][0]
        try:
            raw = bucket.get_object(marker_key).read()
            marker_doc = json.loads(raw.decode("utf-8"))
            marker_status = str(marker_doc.get("status") or "")
            if marker_doc.get("warnings"):
                marker_warnings = [str(w) for w in marker_doc["warnings"]]
        except Exception:
            marker_status = "unknown"

    obj = bucket.get_object(chosen_key)
    data = obj.read()
    truncated = False
    if len(data) > max_bytes:
        data = data[-max_bytes:]
        truncated = True

    try:
        content = data.decode("utf-8", errors="replace")
    except Exception:
        content = data.decode("latin-1", errors="replace")

    warnings: list[str] = [
        "Source: node-agent durable store (OSS). Live tail still uses Lifecycle diagnostics.",
    ]
    if marker_status:
        warnings.append(f"Latest marker status: {marker_status}")
    warnings.extend(marker_warnings)

    return {
        "sandboxId": sandbox_id,
        "kind": "logs",
        "scope": "archive",
        "delivery": "inline",
        "content": content,
        "contentLength": len(data),
        "truncated": truncated,
        "warnings": warnings,
        "archive": {
            "objectKey": chosen_key,
            "prefix": prefix,
            "markerStatus": marker_status,
        },
    }


async def fetch_archive_logs(
    settings: Settings,
    namespace: str,
    sandbox_id: str,
    *,
    max_bytes: int | None = None,
) -> dict[str, Any]:
    if not settings.bff_nodeagent_archive_enabled:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "code": "NODEAGENT_ARCHIVE_DISABLED",
                "message": "Set BFF_NODEAGENT_ARCHIVE_ENABLED=true and configure OSS read credentials",
            },
        )
    if not _oss_configured(settings):
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "code": "NODEAGENT_OSS_NOT_CONFIGURED",
                "message": "BFF node-agent OSS endpoint/bucket/credentials are incomplete",
            },
        )

    limit = max_bytes if max_bytes is not None else settings.bff_nodeagent_archive_max_bytes
    import asyncio

    return await asyncio.to_thread(
        _read_archive_sync,
        settings,
        namespace,
        sandbox_id,
        max_bytes=limit,
    )
