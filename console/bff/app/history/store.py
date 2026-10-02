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
import logging
from datetime import datetime, timedelta, timezone
from typing import Any

from psycopg.rows import dict_row
from psycopg_pool import ConnectionPool

from app.config import Settings
from app.history.resources import (
    explicit_resource_limits,
    limits_to_numbers,
    resource_limits_dict,
)
from app.runtime import attach_runtime_summary

logger = logging.getLogger(__name__)

_SCHEMA_LOCK = "opensandbox-sandbox-lifecycle-history"
_HISTORY_TABLE = "sandbox_lifecycle_history"
_pool: ConnectionPool | None = None

_TERMINAL = frozenset({"Terminated", "Failed", "Stopping"})


def _parse_ts(value: str | None) -> datetime | None:
    if not value:
        return None
    normalized = value.replace("Z", "+00:00")
    try:
        dt = datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def init_history_pool(settings: Settings) -> None:
    global _pool
    if not settings.bff_history_enabled:
        return
    if not settings.bff_history_database_url.strip():
        raise RuntimeError("BFF_HISTORY_ENABLED requires BFF_HISTORY_DATABASE_URL")
    if _pool is not None:
        return
    _pool = ConnectionPool(
        settings.bff_history_database_url,
        min_size=1,
        max_size=5,
        kwargs={"row_factory": dict_row},
    )
    _ensure_schema(_pool)
    logger.info("Console sandbox history pool initialized")


def close_history_pool() -> None:
    global _pool
    if _pool is not None:
        _pool.close()
        _pool = None


def _ensure_schema(pool: ConnectionPool) -> None:
    with pool.connection() as conn:
        conn.execute("SELECT pg_advisory_xact_lock(hashtext(%s))", (_SCHEMA_LOCK,))
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS sandbox_lifecycle_history (
                sandbox_id TEXT PRIMARY KEY,
                tenant_name TEXT NOT NULL,
                namespace TEXT NOT NULL,
                state TEXT,
                image_uri TEXT,
                lifecycle_created_at TIMESTAMPTZ,
                expires_at TIMESTAMPTZ,
                ended_at TIMESTAMPTZ,
                wall_clock_seconds INTEGER,
                source TEXT NOT NULL DEFAULT 'console',
                create_request JSONB,
                first_recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                deleted_at TIMESTAMPTZ
            )
            """
        )
        conn.execute(
            """
            CREATE INDEX IF NOT EXISTS idx_sandbox_lifecycle_history_tenant_created
                ON sandbox_lifecycle_history (tenant_name, lifecycle_created_at DESC NULLS LAST)
            """
        )
        conn.execute(
            """
            CREATE INDEX IF NOT EXISTS idx_sandbox_lifecycle_history_state
                ON sandbox_lifecycle_history (state)
            """
        )
        conn.execute(
            "ALTER TABLE sandbox_lifecycle_history ADD COLUMN IF NOT EXISTS cpu_limit TEXT"
        )
        conn.execute(
            "ALTER TABLE sandbox_lifecycle_history ADD COLUMN IF NOT EXISTS memory_limit TEXT"
        )
        conn.execute(
            "ALTER TABLE sandbox_lifecycle_history ADD COLUMN IF NOT EXISTS cpu_cores DOUBLE PRECISION"
        )
        conn.execute(
            "ALTER TABLE sandbox_lifecycle_history ADD COLUMN IF NOT EXISTS memory_gi DOUBLE PRECISION"
        )


def _image_uri(sandbox: dict[str, Any]) -> str | None:
    image = sandbox.get("image")
    if isinstance(image, dict):
        uri = image.get("uri")
        return str(uri) if uri else None
    if isinstance(image, str):
        return image
    return None


def upsert_from_sandbox(
    settings: Settings,
    *,
    tenant_name: str,
    namespace: str,
    sandbox: dict[str, Any],
    source: str = "console",
    create_request: dict[str, Any] | None = None,
) -> None:
    if not settings.bff_history_enabled or _pool is None:
        return
    sandbox_id = sandbox.get("id")
    if not sandbox_id:
        return

    enriched = attach_runtime_summary(sandbox)
    status = enriched.get("status") or {}
    state = status.get("state")
    wall = (enriched.get("runtimeSummary") or {}).get("wallClockSeconds")
    lifecycle_created = _parse_ts(enriched.get("createdAt"))
    expires = _parse_ts(enriched.get("expiresAt"))
    last_transition = _parse_ts(status.get("lastTransitionAt"))
    ended_at: datetime | None = None
    if state in _TERMINAL and last_transition:
        ended_at = last_transition
    now = datetime.now(timezone.utc)

    with _pool.connection() as conn:
        existing_cr: dict[str, Any] | None = None
        if create_request is None:
            prev = conn.execute(
                "SELECT create_request FROM sandbox_lifecycle_history WHERE sandbox_id = %s",
                (sandbox_id,),
            ).fetchone()
            if prev and prev.get("create_request") is not None:
                raw = prev["create_request"]
                if isinstance(raw, str):
                    try:
                        existing_cr = json.loads(raw)
                    except json.JSONDecodeError:
                        existing_cr = None
                elif isinstance(raw, dict):
                    existing_cr = raw

        merged_create_request = create_request if create_request is not None else existing_cr
        explicit = explicit_resource_limits(enriched, merged_create_request)
        if explicit:
            cpu_limit, memory_limit, cpu_cores, memory_gi = limits_to_numbers(explicit)
        else:
            cpu_limit = memory_limit = None
            cpu_cores = memory_gi = None

        conn.execute(
            """
            INSERT INTO sandbox_lifecycle_history (
                sandbox_id, tenant_name, namespace, state, image_uri,
                lifecycle_created_at, expires_at, ended_at, wall_clock_seconds,
                source, create_request, first_recorded_at, last_seen_at,
                cpu_limit, memory_limit, cpu_cores, memory_gi
            ) VALUES (
                %s, %s, %s, %s, %s,
                %s, %s, %s, %s,
                %s, %s::jsonb, %s, %s,
                %s, %s, %s, %s
            )
            ON CONFLICT (sandbox_id) DO UPDATE SET
                tenant_name = EXCLUDED.tenant_name,
                namespace = EXCLUDED.namespace,
                state = CASE
                    WHEN sandbox_lifecycle_history.deleted_at IS NOT NULL
                    THEN sandbox_lifecycle_history.state
                    WHEN sandbox_lifecycle_history.state IN ('Terminated', 'Failed')
                    THEN sandbox_lifecycle_history.state
                    ELSE EXCLUDED.state
                END,
                image_uri = COALESCE(EXCLUDED.image_uri, sandbox_lifecycle_history.image_uri),
                lifecycle_created_at = CASE
                    WHEN sandbox_lifecycle_history.deleted_at IS NOT NULL
                        OR sandbox_lifecycle_history.state IN ('Terminated', 'Failed')
                    THEN sandbox_lifecycle_history.lifecycle_created_at
                    ELSE COALESCE(
                        EXCLUDED.lifecycle_created_at,
                        sandbox_lifecycle_history.lifecycle_created_at,
                    )
                END,
                expires_at = COALESCE(
                    EXCLUDED.expires_at, sandbox_lifecycle_history.expires_at
                ),
                ended_at = COALESCE(EXCLUDED.ended_at, sandbox_lifecycle_history.ended_at),
                wall_clock_seconds = COALESCE(EXCLUDED.wall_clock_seconds, sandbox_lifecycle_history.wall_clock_seconds),
                last_seen_at = EXCLUDED.last_seen_at,
                create_request = COALESCE(
                    EXCLUDED.create_request, sandbox_lifecycle_history.create_request
                ),
                cpu_limit = COALESCE(EXCLUDED.cpu_limit, sandbox_lifecycle_history.cpu_limit),
                memory_limit = COALESCE(
                    EXCLUDED.memory_limit, sandbox_lifecycle_history.memory_limit
                ),
                cpu_cores = COALESCE(EXCLUDED.cpu_cores, sandbox_lifecycle_history.cpu_cores),
                memory_gi = COALESCE(EXCLUDED.memory_gi, sandbox_lifecycle_history.memory_gi),
                source = CASE
                    WHEN EXCLUDED.source <> 'server-lifecycle' THEN EXCLUDED.source
                    WHEN sandbox_lifecycle_history.source IS NOT NULL
                         AND sandbox_lifecycle_history.source <> 'server-lifecycle'
                    THEN sandbox_lifecycle_history.source
                    ELSE EXCLUDED.source
                END
            """,
            (
                sandbox_id,
                tenant_name,
                namespace,
                state,
                _image_uri(enriched),
                lifecycle_created,
                expires,
                ended_at,
                int(wall) if isinstance(wall, int) else None,
                source,
                json.dumps(create_request) if create_request else None,
                now,
                now,
                cpu_limit,
                memory_limit,
                cpu_cores,
                memory_gi,
            ),
        )


def _row_lifecycle_start(row: dict[str, Any]) -> datetime | None:
    start = row.get("lifecycle_created_at") or row.get("first_recorded_at")
    if isinstance(start, datetime):
        return start.astimezone(timezone.utc)
    return None


def _lifecycle_end_at(
    row: dict[str, Any],
    *,
    period_to: datetime,
    now: datetime,
) -> datetime:
    """End of sandbox lifetime (never extends past destroy/terminate into 'now' unless still active)."""
    for key in ("ended_at", "deleted_at"):
        ts = row.get(key)
        if isinstance(ts, datetime):
            return min(ts.astimezone(timezone.utc), period_to)

    start = _row_lifecycle_start(row)
    state = row.get("state")
    closed = state in _TERMINAL or row.get("deleted_at") is not None
    if closed and start is not None:
        wall = row.get("wall_clock_seconds")
        if isinstance(wall, int) and wall >= 0:
            return min(start + timedelta(seconds=wall), period_to)
        last = row.get("last_seen_at")
        if isinstance(last, datetime):
            return min(last.astimezone(timezone.utc), period_to)

    return min(now, period_to)


def _overlap_seconds_in_period(
    row: dict[str, Any],
    *,
    period_from: datetime,
    period_to: datetime,
    now: datetime,
) -> float:
    """Seconds of sandbox lifetime overlapping [period_from, period_to)."""
    start = row.get("lifecycle_created_at") or row.get("first_recorded_at")
    if not isinstance(start, datetime):
        return 0.0
    start = start.astimezone(timezone.utc)
    end = _lifecycle_end_at(row, period_to=period_to, now=now)
    overlap_start = max(start, period_from)
    overlap_end = min(end, period_to)
    if overlap_end <= overlap_start:
        return 0.0
    return (overlap_end - overlap_start).total_seconds()


def _display_wall_clock_seconds(row: dict[str, Any]) -> int | None:
    start = _row_lifecycle_start(row)
    if start is None:
        wall = row.get("wall_clock_seconds")
        return int(wall) if isinstance(wall, int) else None
    for key in ("ended_at", "deleted_at"):
        ts = row.get(key)
        if isinstance(ts, datetime):
            end = ts.astimezone(timezone.utc)
            return max(0, int((end - start).total_seconds()))
    if row.get("deleted_at") or row.get("state") in _TERMINAL:
        wall = row.get("wall_clock_seconds")
        if isinstance(wall, int):
            return wall
    wall = row.get("wall_clock_seconds")
    return int(wall) if isinstance(wall, int) else None


_WALL_CLOCK_FREEZE_SQL = """
    wall_clock_seconds = COALESCE(
        wall_clock_seconds,
        GREATEST(
            0,
            FLOOR(
                EXTRACT(
                    EPOCH FROM (
                        COALESCE(ended_at, %s) - COALESCE(lifecycle_created_at, first_recorded_at)
                    )
                )
            )::int
        )
    )
"""


def mark_deleted(
    settings: Settings,
    sandbox_id: str,
    *,
    tenant_name: str,
    namespace: str,
    final_sandbox: dict[str, Any] | None = None,
) -> None:
    if not settings.bff_history_enabled or _pool is None:
        return
    now = datetime.now(timezone.utc)
    if final_sandbox and tenant_name:
        st = (final_sandbox.get("status") or {}).get("state")
        if st in _TERMINAL:
            upsert_from_sandbox(
                settings,
                tenant_name=tenant_name,
                namespace=namespace,
                sandbox=final_sandbox,
            )
    with _pool.connection() as conn:
        conn.execute(
            """
            UPDATE sandbox_lifecycle_history
            SET
                deleted_at = COALESCE(deleted_at, %s),
                state = 'Terminated',
                ended_at = COALESCE(ended_at, %s),
                last_seen_at = %s,
            """
            + _WALL_CLOCK_FREEZE_SQL
            + """
            WHERE sandbox_id = %s
            """,
            (now, now, now, now, sandbox_id),
        )


def mark_missing_as_terminated(
    settings: Settings,
    *,
    tenant_name: str,
    live_sandbox_ids: set[str],
) -> int:
    """Close history rows that Lifecycle no longer lists (expired, SDK delete, etc.)."""
    if _pool is None:
        return 0
    now = datetime.now(timezone.utc)
    with _pool.connection() as conn:
        rows = conn.execute(
            """
            SELECT sandbox_id FROM sandbox_lifecycle_history
            WHERE tenant_name = %s
              AND deleted_at IS NULL
              AND (state IS NULL OR state NOT IN ('Terminated', 'Failed', 'Stopping'))
            """,
            (tenant_name,),
        ).fetchall()
        stale = [r["sandbox_id"] for r in rows if r["sandbox_id"] not in live_sandbox_ids]
        if not stale:
            return 0
        conn.execute(
            """
            UPDATE sandbox_lifecycle_history
            SET
                state = 'Terminated',
                ended_at = COALESCE(ended_at, %s),
                deleted_at = COALESCE(deleted_at, %s),
                last_seen_at = %s,
            """
            + _WALL_CLOCK_FREEZE_SQL
            + """
            WHERE tenant_name = %s
              AND sandbox_id = ANY(%s)
            """,
            (now, now, now, now, tenant_name, stale),
        )
        return len(stale)


def list_history(
    settings: Settings,
    *,
    tenant_name: str | None,
    page: int,
    page_size: int,
    include_active: bool = True,
    period_from: datetime | None = None,
    period_to: datetime | None = None,
) -> dict[str, Any]:
    if _pool is None:
        return {"items": [], "pagination": _empty_page(page, page_size)}

    if period_from is not None and period_to is not None:
        period_from = period_from.astimezone(timezone.utc)
        period_to = period_to.astimezone(timezone.utc)
        now = datetime.now(timezone.utc)
        where = "WHERE COALESCE(lifecycle_created_at, first_recorded_at) < %s"
        params: list[Any] = [period_to]
        if tenant_name:
            where += " AND tenant_name = %s"
            params.append(tenant_name)
        with _pool.connection() as conn:
            rows = conn.execute(
                f"""
                SELECT
                    h.*,
                    (SELECT COUNT(*)::int FROM snapshots s WHERE s.source_sandbox_id = h.sandbox_id) AS snapshot_count
                FROM sandbox_lifecycle_history h
                {where}
                """,
                params,
            ).fetchall()
        matched = [
            r
            for r in rows
            if _overlap_seconds_in_period(r, period_from=period_from, period_to=period_to, now=now) > 0
        ]
        if not include_active:
            matched = [
                r
                for r in matched
                if r.get("deleted_at") is not None or r.get("state") in _TERMINAL
            ]
        def _history_sort_key(row: dict[str, Any]) -> datetime:
            start = row.get("lifecycle_created_at") or row.get("first_recorded_at")
            if isinstance(start, datetime):
                return start.astimezone(timezone.utc)
            return datetime.min.replace(tzinfo=timezone.utc)

        matched.sort(key=_history_sort_key, reverse=True)
        total = len(matched)
        offset = (page - 1) * page_size
        page_rows = matched[offset : offset + page_size]
        items = [_row_to_item(r) for r in page_rows]
        total_pages = max(1, (total + page_size - 1) // page_size) if total else 1
        return {
            "items": items,
            "period": {
                "from": period_from.isoformat().replace("+00:00", "Z"),
                "to": period_to.isoformat().replace("+00:00", "Z"),
            },
            "pagination": {
                "page": page,
                "pageSize": page_size,
                "totalItems": total,
                "totalPages": total_pages,
                "hasNextPage": page < total_pages,
            },
        }

    offset = (page - 1) * page_size
    where = "WHERE 1=1"
    params = []
    if tenant_name:
        where += " AND h.tenant_name = %s"
        params.append(tenant_name)
    if not include_active:
        where += " AND (h.deleted_at IS NOT NULL OR h.state = ANY(%s))"
        params.append(list(_TERMINAL))

    with _pool.connection() as conn:
        total = conn.execute(
            f"SELECT COUNT(*) AS c FROM sandbox_lifecycle_history h {where}",
            params,
        ).fetchone()["c"]
        rows = conn.execute(
            f"""
            SELECT
                h.*,
                (SELECT COUNT(*)::int FROM snapshots s WHERE s.source_sandbox_id = h.sandbox_id) AS snapshot_count
            FROM sandbox_lifecycle_history h
            {where}
            ORDER BY COALESCE(h.lifecycle_created_at, h.first_recorded_at) DESC
            LIMIT %s OFFSET %s
            """,
            [*params, page_size, offset],
        ).fetchall()

    items = [_row_to_item(r) for r in rows]
    total_pages = max(1, (total + page_size - 1) // page_size)
    return {
        "items": items,
        "pagination": {
            "page": page,
            "pageSize": page_size,
            "totalItems": total,
            "totalPages": total_pages,
            "hasNextPage": page < total_pages,
        },
    }


def get_history_record(
    settings: Settings,
    *,
    sandbox_id: str,
    tenant_name: str | None,
) -> dict[str, Any] | None:
    if _pool is None:
        return None
    where = "WHERE h.sandbox_id = %s"
    params: list[Any] = [sandbox_id]
    if tenant_name:
        where += " AND h.tenant_name = %s"
        params.append(tenant_name)
    with _pool.connection() as conn:
        row = conn.execute(
            f"""
            SELECT
                h.*,
                (SELECT COUNT(*)::int FROM snapshots s WHERE s.source_sandbox_id = h.sandbox_id) AS snapshot_count
            FROM sandbox_lifecycle_history h
            {where}
            """,
            params,
        ).fetchone()
    if not row:
        return None
    return _row_to_item(row)


def history_stats(settings: Settings, *, tenant_name: str | None) -> dict[str, Any]:
    if _pool is None:
        return {"enabled": False}
    where = ""
    params: list[Any] = []
    if tenant_name:
        where = "WHERE tenant_name = %s"
        params.append(tenant_name)
    with _pool.connection() as conn:
        row = conn.execute(
            f"""
            SELECT
                COUNT(*)::int AS total_records,
                COUNT(*) FILTER (WHERE deleted_at IS NULL AND state NOT IN ('Terminated','Failed','Stopping'))::int AS active_records,
                COALESCE(SUM(wall_clock_seconds) FILTER (WHERE wall_clock_seconds IS NOT NULL), 0)::bigint AS total_wall_clock_seconds,
                COALESCE(AVG(wall_clock_seconds) FILTER (WHERE wall_clock_seconds IS NOT NULL), 0)::int AS avg_wall_clock_seconds
            FROM sandbox_lifecycle_history
            {where}
            """,
            params,
        ).fetchone()
    return {
        "enabled": True,
        "asOf": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "totalRecords": int(row["total_records"]),
        "activeRecords": int(row["active_records"]),
        "totalWallClockSeconds": int(row["total_wall_clock_seconds"]),
        "avgWallClockSeconds": int(row["avg_wall_clock_seconds"]),
    }


_UNKNOWN_IMAGE_LABEL = "(未记录镜像)"


def image_stats(
    settings: Settings,
    *,
    tenant_name: str | None,
    period_from: datetime | None = None,
    period_to: datetime | None = None,
) -> dict[str, Any]:
    """Aggregate sandbox counts by base image URI from sandbox_lifecycle_history."""
    if _pool is None:
        return {"enabled": False, "items": []}
    where_parts = ["1=1"]
    params: list[Any] = []
    if tenant_name:
        where_parts.append("tenant_name = %s")
        params.append(tenant_name)
    if period_from is not None:
        where_parts.append("COALESCE(lifecycle_created_at, first_recorded_at) >= %s")
        params.append(period_from)
    if period_to is not None:
        where_parts.append("COALESCE(lifecycle_created_at, first_recorded_at) < %s")
        params.append(period_to)
    where_sql = " AND ".join(where_parts)

    with _pool.connection() as conn:
        rows = conn.execute(
            f"""
            SELECT
                COALESCE(NULLIF(TRIM(image_uri), ''), %s) AS image_uri,
                COUNT(*)::int AS sandbox_count,
                COUNT(*) FILTER (
                    WHERE deleted_at IS NULL
                      AND (state IS NULL OR state NOT IN ('Terminated','Failed','Stopping'))
                )::int AS active_count,
                COUNT(DISTINCT tenant_name)::int AS tenant_count,
                MAX(COALESCE(lifecycle_created_at, first_recorded_at)) AS last_used_at
            FROM sandbox_lifecycle_history
            WHERE {where_sql}
            GROUP BY 1
            ORDER BY sandbox_count DESC, image_uri ASC
            """,
            [_UNKNOWN_IMAGE_LABEL, *params],
        ).fetchall()

    total = sum(int(r["sandbox_count"]) for r in rows)

    def iso_dt(v: Any) -> str | None:
        if v is None:
            return None
        if isinstance(v, datetime):
            return v.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
        return str(v)

    items: list[dict[str, Any]] = []
    for row in rows:
        count = int(row["sandbox_count"])
        items.append(
            {
                "imageUri": row["image_uri"],
                "sandboxCount": count,
                "activeCount": int(row["active_count"]),
                "tenantCount": int(row["tenant_count"]),
                "sharePercent": round(count / total * 100, 2) if total else 0.0,
                "lastUsedAt": iso_dt(row["last_used_at"]),
            }
        )

    return {
        "enabled": True,
        "asOf": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "totalRecords": total,
        "distinctImages": len(items),
        "items": items,
    }


def _row_cpu_memory(row: dict[str, Any]) -> tuple[float, float]:
    cpu = row.get("cpu_cores")
    mem = row.get("memory_gi")
    if cpu is not None and mem is not None:
        return float(cpu), float(mem)
    cr = row.get("create_request")
    if isinstance(cr, str):
        try:
            cr = json.loads(cr)
        except json.JSONDecodeError:
            cr = None
    limits = resource_limits_dict({}, cr if isinstance(cr, dict) else None)
    _, _, cpu, mem = limits_to_numbers(limits)
    return cpu, mem


def usage_allocation(
    settings: Settings,
    *,
    period_from: datetime,
    period_to: datetime,
    display_tenant: str | None = None,
    viewer_tenant_only: str | None = None,
) -> dict[str, Any]:
    """Aggregate tenant usage in [period_from, period_to) using resource limits."""
    if _pool is None:
        return {"enabled": False}
    if period_to <= period_from:
        raise ValueError("period to must be after from")

    period_from = period_from.astimezone(timezone.utc)
    period_to = period_to.astimezone(timezone.utc)
    now = datetime.now(timezone.utc)

    with _pool.connection() as conn:
        rows = conn.execute(
            """
            SELECT
                sandbox_id, tenant_name, state,
                lifecycle_created_at, first_recorded_at,
                ended_at, deleted_at, last_seen_at, wall_clock_seconds,
                cpu_cores, memory_gi, create_request
            FROM sandbox_lifecycle_history
            WHERE COALESCE(lifecycle_created_at, first_recorded_at) < %s
            """,
            (period_to,),
        ).fetchall()

    by_tenant: dict[str, dict[str, float | int]] = {}

    def bucket(t: str) -> dict[str, float | int]:
        if t not in by_tenant:
            by_tenant[t] = {
                "sandboxCount": 0,
                "overlapSeconds": 0.0,
                "cpuCoreSeconds": 0.0,
                "memoryGiSeconds": 0.0,
            }
        return by_tenant[t]

    for row in rows:
        seconds = _overlap_seconds_in_period(row, period_from=period_from, period_to=period_to, now=now)
        if seconds <= 0:
            continue
        cpu, mem = _row_cpu_memory(row)
        t = str(row["tenant_name"])
        b = bucket(t)
        b["sandboxCount"] = int(b["sandboxCount"]) + 1
        b["overlapSeconds"] = float(b["overlapSeconds"]) + seconds
        b["cpuCoreSeconds"] = float(b["cpuCoreSeconds"]) + cpu * seconds
        b["memoryGiSeconds"] = float(b["memoryGiSeconds"]) + mem * seconds

    cluster_totals = {
        "sandboxCount": 0,
        "overlapSeconds": 0.0,
        "cpuCoreSeconds": 0.0,
        "memoryGiSeconds": 0.0,
    }
    for b in by_tenant.values():
        cluster_totals["sandboxCount"] += int(b["sandboxCount"])
        cluster_totals["overlapSeconds"] += float(b["overlapSeconds"])
        cluster_totals["cpuCoreSeconds"] += float(b["cpuCoreSeconds"])
        cluster_totals["memoryGiSeconds"] += float(b["memoryGiSeconds"])

    def _tenant_in_scope(tenant: str) -> bool:
        if viewer_tenant_only and tenant != viewer_tenant_only:
            return False
        if display_tenant and tenant != display_tenant:
            return False
        return True

    totals = {
        "sandboxCount": 0,
        "overlapSeconds": 0.0,
        "cpuCoreSeconds": 0.0,
        "memoryGiSeconds": 0.0,
    }
    for t, b in by_tenant.items():
        if not _tenant_in_scope(t):
            continue
        totals["sandboxCount"] += int(b["sandboxCount"])
        totals["overlapSeconds"] += float(b["overlapSeconds"])
        totals["cpuCoreSeconds"] += float(b["cpuCoreSeconds"])
        totals["memoryGiSeconds"] += float(b["memoryGiSeconds"])

    def pct(part: float, whole: float) -> float:
        if whole <= 0:
            return 0.0
        return round(100.0 * part / whole, 2)

    def composite_share_pct(b: dict[str, float | int]) -> float:
        """Equal-weight mean of time / CPU / memory shares (each sums to 100% cluster-wide)."""
        terms: list[float] = []
        for key in ("overlapSeconds", "cpuCoreSeconds", "memoryGiSeconds"):
            whole = float(cluster_totals[key])
            if whole > 0:
                terms.append(float(b[key]) / whole)
        if not terms:
            return 0.0
        return round(100.0 * sum(terms) / len(terms), 2)

    tenants_out: list[dict[str, Any]] = []
    for t, b in sorted(by_tenant.items(), key=lambda x: (-float(x[1]["overlapSeconds"]), x[0])):
        if viewer_tenant_only and t != viewer_tenant_only:
            continue
        if display_tenant and t != display_tenant:
            continue
        tenants_out.append(
            {
                "tenant": t,
                "sandboxCount": int(b["sandboxCount"]),
                "overlapSeconds": int(round(float(b["overlapSeconds"]))),
                "cpuCoreSeconds": round(float(b["cpuCoreSeconds"]), 2),
                "memoryGiSeconds": round(float(b["memoryGiSeconds"]), 2),
                "sharePercent": {
                    "time": pct(float(b["overlapSeconds"]), float(cluster_totals["overlapSeconds"])),
                    "cpu": pct(float(b["cpuCoreSeconds"]), float(cluster_totals["cpuCoreSeconds"])),
                    "memory": pct(float(b["memoryGiSeconds"]), float(cluster_totals["memoryGiSeconds"])),
                    "composite": composite_share_pct(b),
                },
            }
        )

    def iso(dt: datetime) -> str:
        return dt.isoformat().replace("+00:00", "Z")

    return {
        "enabled": True,
        "basis": "resourceLimits",
        "coverageNote": (
            "沙箱数 = 与统计区间有生命周期交集的 history 条数（与占用时长同一批实例，非实时列表、非申请历史总条数）。"
            "占用时长 = 各沙箱「创建→结束」与区间的交集秒数之和（结束取 ended_at/deleted_at 或已终止 wall_clock，"
            "不会把已销毁沙箱延续到当前时间）。统计 sandbox_lifecycle_history 单表（Server 审计 + Console 扩展）。"
        ),
        "shareFormula": {
            "time": "时长占比 = 租户占用秒数 ÷ 区间全集群占用秒数 × 100%",
            "cpu": "CPU 占比 = 租户 CPU·秒 ÷ 全集群 CPU·秒 × 100%（CPU·秒 = Σ limits.cpu × 占用秒）",
            "memory": "内存占比 = 租户内存·Gi·秒 ÷ 全集群内存·Gi·秒 × 100%",
            "composite": (
                "综合分摊占比 = 平均(时长占比, CPU占比, 内存占比) 的小数形式后再 ×100，"
                "即 (T_i/T_tot + C_i/C_tot + M_i/M_tot) / k × 100%，k 为分母>0 的维度数（通常为 3）。"
                "各租户 composite 之和为 100%。后续可改为按核/Gi 单价加权。"
            ),
        },
        "period": {"from": iso(period_from), "to": iso(period_to)},
        "totals": {
            "sandboxCount": totals["sandboxCount"],
            "overlapSeconds": int(round(totals["overlapSeconds"])),
            "cpuCoreSeconds": round(totals["cpuCoreSeconds"], 2),
            "memoryGiSeconds": round(totals["memoryGiSeconds"], 2),
        },
        "tenants": tenants_out,
    }


def _limits_payload(row: dict[str, Any]) -> dict[str, Any]:
    """Expose create-time resourceLimits from history columns + stored create_request."""
    limits: dict[str, str] = {}
    if row.get("cpu_limit"):
        limits["cpu"] = str(row["cpu_limit"])
    if row.get("memory_limit"):
        limits["memory"] = str(row["memory_limit"])

    create_request = row.get("create_request")
    if isinstance(create_request, str):
        try:
            create_request = json.loads(create_request)
        except json.JSONDecodeError:
            create_request = None

    requests: dict[str, str] | None = None
    timeout: int | None = None
    if isinstance(create_request, dict):
        rl = create_request.get("resourceLimits")
        if isinstance(rl, dict):
            for key, val in rl.items():
                if val is not None:
                    limits[str(key)] = str(val)
        rr = create_request.get("resourceRequests")
        if isinstance(rr, dict) and rr:
            requests = {str(k): str(v) for k, v in rr.items() if v is not None}
        raw_timeout = create_request.get("timeout")
        if isinstance(raw_timeout, (int, float)):
            timeout = int(raw_timeout)

    payload: dict[str, Any] = {}
    if limits:
        payload["resourceLimits"] = limits
        _, _, cpu_from_limits, mem_from_limits = limits_to_numbers(limits)
        payload["cpuCores"] = cpu_from_limits
        payload["memoryGi"] = mem_from_limits
    elif row.get("cpu_cores") is not None and row.get("memory_gi") is not None:
        payload["cpuCores"] = float(row["cpu_cores"])
        payload["memoryGi"] = float(row["memory_gi"])
    if requests:
        payload["resourceRequests"] = requests
    if timeout is not None:
        payload["createTimeoutSeconds"] = timeout
    return payload


def _row_to_item(row: dict[str, Any]) -> dict[str, Any]:
    def iso(v: Any) -> str | None:
        if v is None:
            return None
        if isinstance(v, datetime):
            return v.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
        return str(v)

    raw_state = row["state"]
    if row.get("deleted_at") and raw_state not in _TERMINAL:
        raw_state = "Terminated"

    return {
        "sandboxId": row["sandbox_id"],
        "tenant": row["tenant_name"],
        "namespace": row["namespace"],
        "state": raw_state,
        "imageUri": row["image_uri"],
        "createdAt": iso(row["lifecycle_created_at"]),
        "expiresAt": iso(row["expires_at"]),
        "endedAt": iso(row["ended_at"]),
        "wallClockSeconds": _display_wall_clock_seconds(row),
        "deletedAt": iso(row["deleted_at"]),
        "firstRecordedAt": iso(row["first_recorded_at"]),
        "lastSeenAt": iso(row["last_seen_at"]),
        "source": row["source"],
        "snapshotCount": row.get("snapshot_count", 0),
        **_limits_payload(row),
    }


def _empty_page(page: int, page_size: int) -> dict[str, Any]:
    return {
        "page": page,
        "pageSize": page_size,
        "totalItems": 0,
        "totalPages": 1,
        "hasNextPage": False,
    }
