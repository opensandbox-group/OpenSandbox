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

"""PostgreSQL storage for sandbox access audit events.

Two tables are maintained:

- ``sandbox_access_log``: one row per received request (request detail).
- ``sandbox_access_latest``: one row per sandbox id, holding the latest
  request and the total request count (summary). Rows whose sandbox
  resource is gone are flagged ``deleted`` and hidden from the UI/API.
  Sandboxes discovered in the cluster before any request arrived are
  inserted with ``accessed = FALSE`` (request fields NULL) so the UI can
  tell them apart from accessed ones.
"""

import logging
from datetime import datetime, timezone

from psycopg import sql
from psycopg.rows import dict_row
from psycopg_pool import ConnectionPool

logger = logging.getLogger(__name__)

# One row per access request.
_CREATE_DETAIL_TABLE = """
CREATE TABLE IF NOT EXISTS sandbox_access_log (
    id           BIGSERIAL PRIMARY KEY,
    sandbox_id   TEXT        NOT NULL,
    uri          TEXT        NOT NULL,
    method       TEXT        NOT NULL,
    target       TEXT        NOT NULL,
    request_time TIMESTAMPTZ NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT date_trunc('second', now())
);
CREATE INDEX IF NOT EXISTS idx_sandbox_access_log_sandbox_time
    ON sandbox_access_log (sandbox_id, request_time DESC);
"""

# One row per sandbox id, tracking its latest request and total count.
# ``deleted`` marks sandboxes whose BatchSandbox resource no longer exists
# (synced from Kubernetes); deleted rows are hidden from the UI/API.
# ``accessed`` is FALSE on rows inserted by the cluster discovery for
# sandboxes that have not been accessed yet - their request fields are
# NULL until the first audit event arrives.
# ``created_at`` holds the sandbox's BatchSandbox creationTimestamp on
# discovery-inserted rows; ``node_ip`` is the IP of the node the sandbox
# pod runs on (synced from the cluster).
_CREATE_SUMMARY_TABLE = """
CREATE TABLE IF NOT EXISTS sandbox_access_latest (
    sandbox_id    TEXT        PRIMARY KEY,
    uri           TEXT,
    method        TEXT,
    target        TEXT,
    request_time  TIMESTAMPTZ,
    request_count BIGINT      NOT NULL DEFAULT 1,
    deleted       BOOLEAN     NOT NULL DEFAULT FALSE,
    accessed      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ,
    node_ip       TEXT,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT date_trunc('second', now())
);
"""

# Migrations for tables created before the ``deleted``/``accessed``/
# ``created_at``/``node_ip`` columns existed; the request columns must be
# nullable to hold unaccessed rows.
_ALTER_SUMMARY_MIGRATIONS = """
ALTER TABLE sandbox_access_latest
    ADD COLUMN IF NOT EXISTS deleted BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS accessed BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS node_ip TEXT;
ALTER TABLE sandbox_access_latest
    ALTER COLUMN uri DROP NOT NULL,
    ALTER COLUMN method DROP NOT NULL,
    ALTER COLUMN target DROP NOT NULL,
    ALTER COLUMN request_time DROP NOT NULL
"""

# Truncate timestamps to whole seconds on write.
_TRUNCATED_NOW = "date_trunc('second', now())"

_INSERT_DETAIL = """
INSERT INTO sandbox_access_log (sandbox_id, uri, method, target, request_time, received_at)
VALUES (%(sandbox_id)s, %(uri)s, %(method)s, %(target)s, %(request_time)s, {now})
""".format(now=_TRUNCATED_NOW)

# Upsert the summary row. The WHERE clause guards against out-of-order
# events: an older event never overwrites a newer summary. A NULL
# request_time (row created by the pod discovery, never accessed) loses
# the comparison, so it is handled explicitly.
_UPSERT_SUMMARY = """
INSERT INTO sandbox_access_latest
    (sandbox_id, uri, method, target, request_time, request_count, accessed, updated_at)
VALUES
    (%(sandbox_id)s, %(uri)s, %(method)s, %(target)s, %(request_time)s, 1, TRUE, {now})
ON CONFLICT (sandbox_id) DO UPDATE SET
    uri           = EXCLUDED.uri,
    method        = EXCLUDED.method,
    target        = EXCLUDED.target,
    request_time  = EXCLUDED.request_time,
    request_count = sandbox_access_latest.request_count + 1,
    accessed      = TRUE,
    updated_at    = {now}
WHERE EXCLUDED.request_time >= sandbox_access_latest.request_time
   OR sandbox_access_latest.request_time IS NULL
""".format(now=_TRUNCATED_NOW)

# Placeholder rows for sandboxes discovered in the cluster before any
# request arrived. Rows that already exist get their ``created_at``
# backfilled when still NULL (e.g. the sandbox was accessed before the
# first sync ran) - everything else is left untouched. One statement for
# all ids (the DB may be a high-latency round trip away); RETURNING
# splits inserted vs. backfilled rows (``xmax = 0`` marks fresh inserts).
_INSERT_DISCOVERED = """
INSERT INTO sandbox_access_latest
    (sandbox_id, uri, method, target, request_time, request_count, accessed, created_at, updated_at)
SELECT name, NULL, NULL, NULL, NULL, 0, FALSE, created_at, {now}
FROM unnest(%(ids)s::text[], %(created)s::timestamptz[]) AS discovered(name, created_at)
ON CONFLICT (sandbox_id) DO UPDATE SET
    created_at = EXCLUDED.created_at
WHERE sandbox_access_latest.created_at IS NULL
  AND EXCLUDED.created_at IS NOT NULL
RETURNING (xmax = 0) AS inserted
""".format(now=_TRUNCATED_NOW)

# The window count piggybacks the total on the listing query so a page
# load costs one round trip instead of two; the plain COUNT remains as a
# fallback for pages past the end (no rows returned -> no total known).
_LIST_LATEST = """
SELECT sandbox_id, uri, method, target, request_time, request_count, accessed, created_at, node_ip,
       count(*) OVER () AS __total
FROM sandbox_access_latest
WHERE NOT deleted {extra}
ORDER BY {order}
LIMIT %(limit)s OFFSET %(offset)s
"""

_COUNT_LATEST = """
SELECT count(*) AS total
FROM sandbox_access_latest
WHERE NOT deleted {extra}
"""

# Whitelisted sort orders for the summary table. Never-accessed rows have
# a NULL request_time and always sort last within an accessed group;
# discovered rows without a creation timestamp always sort last.
_LATEST_ORDERS = {
    "request_time": "request_time ASC",
    "-request_time": "request_time DESC NULLS LAST",
    "request_count": "request_count ASC",
    "-request_count": "request_count DESC",
    "accessed": "accessed ASC, request_time DESC NULLS LAST",
    "-accessed": "accessed DESC, request_time DESC NULLS LAST",
    "created_at": "created_at ASC NULLS LAST",
    "-created_at": "created_at DESC NULLS LAST",
}

_LIST_DETAILS = """
SELECT id, sandbox_id, uri, method, target, request_time, received_at,
       count(*) OVER () AS __total
FROM sandbox_access_log
WHERE {where}
ORDER BY id DESC
LIMIT %(limit)s OFFSET %(offset)s
"""

_COUNT_DETAILS = """
SELECT count(*) AS total
FROM sandbox_access_log
WHERE {where}
"""


class AuditStore:
    """Writes audit events to PostgreSQL using a connection pool."""

    def __init__(self, pool: ConnectionPool):
        self.pool = pool

    def init_schema(self) -> None:
        """Create tables and indexes if they do not exist (idempotent)."""
        with self.pool.connection() as conn:
            with conn.cursor() as cur:
                cur.execute(sql.SQL(_CREATE_DETAIL_TABLE))
                cur.execute(sql.SQL(_CREATE_SUMMARY_TABLE))
                cur.execute(sql.SQL(_ALTER_SUMMARY_MIGRATIONS))
        logger.info("audit schema initialized")

    def record(self, events: list[dict]) -> int:
        """Persist events in a single transaction.

        Each event writes one detail row and upserts the sandbox summary
        row. Returns the number of recorded events.
        """
        if not events:
            return 0

        with self.pool.connection() as conn:
            with conn.cursor() as cur:
                for event in events:
                    params = _normalize(event)
                    cur.execute(_INSERT_DETAIL, params)
                    cur.execute(_UPSERT_SUMMARY, params)
        return len(events)

    def list_latest(
        self,
        search: str | None = None,
        sort: str = "-request_time",
        limit: int = 50,
        offset: int = 0,
        time_from: datetime | None = None,
        time_to: datetime | None = None,
    ) -> dict:
        """List per-sandbox latest requests, sorted by ``sort``.

        Rows marked deleted (sandbox resource gone) are excluded.
        ``search`` matches sandbox ids by substring (case-insensitive,
        fuzzy) OR node IPs exactly - typing an IP returns every sandbox
        on that node. ``time_from``/``time_to`` bound the latest request
        time (inclusive; naive timestamps are assumed to be UTC). ``sort``
        is a whitelisted key from ``_LATEST_ORDERS`` (``-`` prefix means
        descending); default is newest first.
        """
        try:
            order = _LATEST_ORDERS[sort]
        except KeyError:
            raise ValueError(f"invalid sort key: {sort}") from None

        params: dict = {"limit": limit, "offset": offset}
        conditions = []
        if search:
            # Fuzzy match on sandbox_id OR exact match on node_ip; escape
            # LIKE wildcards in the input so user input is matched
            # literally. node_ip is a plain equality (IPs are not fuzzy).
            params["pattern"] = f"%{_like_escape(search)}%"
            params["node_ip"] = search
            conditions.append(
                "(sandbox_id ILIKE %(pattern)s ESCAPE '\\'"
                " OR node_ip = %(node_ip)s)"
            )
        if time_from is not None:
            params["time_from"] = _ensure_utc(time_from)
            conditions.append("request_time >= %(time_from)s")
        if time_to is not None:
            params["time_to"] = _ensure_utc(time_to)
            conditions.append("request_time <= %(time_to)s")
        extra = f" AND {' AND '.join(conditions)}" if conditions else ""

        with self.pool.connection() as conn:
            conn.row_factory = dict_row
            with conn.cursor() as cur:
                cur.execute(_LIST_LATEST.format(extra=extra, order=order), params)
                rows = cur.fetchall()
                if rows:
                    total = rows[0]["__total"]
                    rows = [_drop_total(row) for row in rows]
                else:
                    # Past the last page: the window count returned nothing,
                    # fall back to a separate COUNT for the true total.
                    cur.execute(_COUNT_LATEST.format(extra=extra), params)
                    total = cur.fetchone()["total"]
        return {"total": total, "items": [_jsonify(row) for row in rows]}

    def list_details(
        self, sandbox_id: str | None = None, limit: int = 50, offset: int = 0
    ) -> dict:
        """List access request details, newest first, optionally filtered by sandbox."""
        # Build the WHERE clause dynamically: psycopg cannot infer the type
        # of a NULL-bound parameter, and skipping the filter entirely lets
        # the query planner use the (sandbox_id, request_time) index.
        if sandbox_id:
            where, params = "sandbox_id = %(sandbox_id)s", {"sandbox_id": sandbox_id}
        else:
            where, params = "TRUE", {}
        params.update({"limit": limit, "offset": offset})

        with self.pool.connection() as conn:
            conn.row_factory = dict_row
            with conn.cursor() as cur:
                cur.execute(_LIST_DETAILS.format(where=where), params)
                rows = cur.fetchall()
                if rows:
                    total = rows[0]["__total"]
                    rows = [_drop_total(row) for row in rows]
                else:
                    # Past the last page: the window count returned nothing,
                    # fall back to a separate COUNT for the true total.
                    cur.execute(_COUNT_DETAILS.format(where=where), params)
                    total = cur.fetchone()["total"]
        return {"total": total, "items": [_jsonify(row) for row in rows]}

    def upsert_discovered_sandboxes(self, sandboxes: list[dict]) -> dict:
        """Insert placeholder rows for unaccessed sandboxes and backfill
        creation timestamps.

        ``sandboxes`` are the live BatchSandbox resources, each a dict
        with ``name`` (the sandbox id) and ``created_at`` (the resource's
        creationTimestamp, or None).

        - Ids with no summary row get one marked ``accessed = FALSE``
          with NULL request fields, a zero request count and
          ``created_at`` set (existing rows are otherwise untouched).
        - Existing rows whose ``created_at`` is still NULL (e.g. the
          sandbox was accessed before the first sync ran) get it
          backfilled from the resource's creationTimestamp.

        Returns ``{"discovered": <n>, "backfilled": <n>}``.
        """
        # One statement for all ids - each round trip to a remote
        # database can be slow.
        deduped = {sandbox["name"]: sandbox.get("created_at") for sandbox in sandboxes}
        if not deduped:
            return {"discovered": 0, "backfilled": 0}
        with self.pool.connection() as conn:
            conn.row_factory = dict_row
            with conn.cursor() as cur:
                cur.execute(
                    _INSERT_DISCOVERED,
                    {
                        "ids": list(deduped),
                        "created": [
                            _ensure_utc(created) if created is not None else None
                            for created in deduped.values()
                        ],
                    },
                )
                flags = [row["inserted"] for row in cur.fetchall()]
        return {
            "discovered": sum(1 for flag in flags if flag),
            "backfilled": sum(1 for flag in flags if not flag),
        }

    def update_node_ips(self, nodes: dict[str, str]) -> int:
        """Refresh ``node_ip`` from the sandbox pods' host IPs.

        ``nodes`` maps sandbox id -> node IP (from the cluster). Rows
        whose id is present get ``node_ip`` set (a rescheduled pod's new
        node overwrites the old value); ids absent from the map are left
        as they are (the pod may be gone or not scheduled yet). Returns
        the number of rows whose value actually changed.
        """
        if not nodes:
            return 0
        # One statement for all ids - each round trip to a remote
        # database can be slow.
        with self.pool.connection() as conn:
            with conn.cursor() as cur:
                cur.execute(
                    """
                    UPDATE sandbox_access_latest AS latest
                    SET node_ip = mapped.node_ip
                    FROM unnest(%(ids)s::text[], %(ips)s::text[])
                        AS mapped(sandbox_id, node_ip)
                    WHERE latest.sandbox_id = mapped.sandbox_id
                      AND latest.node_ip IS DISTINCT FROM mapped.node_ip
                    """,
                    {"ids": list(nodes), "ips": list(nodes.values())},
                )
                return cur.rowcount

    def sync_deleted_flags(self, live_ids: list[str]) -> dict:
        """Reconcile the ``deleted`` flag against live sandbox resources.

        ``live_ids`` are the names of the BatchSandbox resources currently
        existing in the cluster. Summary rows whose sandbox id is not among
        them are marked deleted (hidden from the UI/API); rows previously
        marked deleted whose id reappears are restored. ``updated_at`` is
        left untouched - it reflects the last request, not the sync.

        Returns ``{"deleted": <n>, "restored": <n>}`` counting rows whose
        flag actually changed.
        """
        with self.pool.connection() as conn:
            with conn.cursor() as cur:
                # An empty live list marks everything deleted; <> ALL ([])
                # is TRUE for every row, so no special case is needed.
                cur.execute(
                    """
                    UPDATE sandbox_access_latest
                    SET deleted = TRUE
                    WHERE NOT deleted AND sandbox_id <> ALL(%(ids)s)
                    """,
                    {"ids": live_ids},
                )
                deleted = cur.rowcount
                cur.execute(
                    """
                    UPDATE sandbox_access_latest
                    SET deleted = FALSE
                    WHERE deleted AND sandbox_id = ANY(%(ids)s)
                    """,
                    {"ids": live_ids},
                )
                restored = cur.rowcount
        return {"deleted": deleted, "restored": restored}


def _normalize(event: dict) -> dict:
    """Coerce an audit event to DB row parameters.

    The ingress sends RFC3339 timestamps with a timezone offset; naive
    timestamps (if any) are assumed to be UTC. Timestamps are truncated
    to whole seconds.
    """
    request_time = event["request_time"]
    if isinstance(request_time, str):
        request_time = datetime.fromisoformat(
            request_time.replace("Z", "+00:00")
        )
    if request_time.tzinfo is None:
        request_time = request_time.replace(tzinfo=timezone.utc)
    request_time = request_time.replace(microsecond=0)

    return {
        "sandbox_id": event["sandbox_id"],
        "uri": event["uri"],
        "method": event["method"],
        "target": event["target"],
        "request_time": request_time,
    }


def _jsonify(row: dict) -> dict:
    """Make a DB row JSON-serializable (datetime -> ISO 8601, seconds only)."""
    return {
        key: value.replace(microsecond=0).isoformat()
        if isinstance(value, datetime)
        else value
        for key, value in row.items()
    }


def _drop_total(row: dict) -> dict:
    """Strip the piggybacked window ``__total`` from a listing row."""
    return {key: value for key, value in row.items() if key != "__total"}


def _ensure_utc(value: datetime) -> datetime:
    """Treat naive filter timestamps as UTC (matches ``_normalize``)."""
    return value if value.tzinfo is not None else value.replace(tzinfo=timezone.utc)


def _like_escape(text: str) -> str:
    """Escape LIKE/ILIKE wildcards so the search input matches literally."""
    return (
        text.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    )
