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
  request and the total request count (summary).
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
_CREATE_SUMMARY_TABLE = """
CREATE TABLE IF NOT EXISTS sandbox_access_latest (
    sandbox_id    TEXT        PRIMARY KEY,
    uri           TEXT        NOT NULL,
    method        TEXT        NOT NULL,
    target        TEXT        NOT NULL,
    request_time  TIMESTAMPTZ NOT NULL,
    request_count BIGINT      NOT NULL DEFAULT 1,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT date_trunc('second', now())
);
"""

# Truncate timestamps to whole seconds on write.
_TRUNCATED_NOW = "date_trunc('second', now())"

_INSERT_DETAIL = """
INSERT INTO sandbox_access_log (sandbox_id, uri, method, target, request_time, received_at)
VALUES (%(sandbox_id)s, %(uri)s, %(method)s, %(target)s, %(request_time)s, {now})
""".format(now=_TRUNCATED_NOW)

# Upsert the summary row. The WHERE clause guards against out-of-order
# events: an older event never overwrites a newer summary.
_UPSERT_SUMMARY = """
INSERT INTO sandbox_access_latest
    (sandbox_id, uri, method, target, request_time, request_count, updated_at)
VALUES
    (%(sandbox_id)s, %(uri)s, %(method)s, %(target)s, %(request_time)s, 1, {now})
ON CONFLICT (sandbox_id) DO UPDATE SET
    uri           = EXCLUDED.uri,
    method        = EXCLUDED.method,
    target        = EXCLUDED.target,
    request_time  = EXCLUDED.request_time,
    request_count = sandbox_access_latest.request_count + 1,
    updated_at    = {now}
WHERE EXCLUDED.request_time >= sandbox_access_latest.request_time
""".format(now=_TRUNCATED_NOW)

_LIST_LATEST = """
SELECT sandbox_id, uri, method, target, request_time, request_count
FROM sandbox_access_latest
{where}
ORDER BY {order}
LIMIT %(limit)s OFFSET %(offset)s
"""

_COUNT_LATEST = """
SELECT count(*) AS total
FROM sandbox_access_latest
{where}
"""

# Whitelisted sort orders for the summary table.
_LATEST_ORDERS = {
    "request_time": "request_time ASC",
    "-request_time": "request_time DESC",
    "request_count": "request_count ASC",
    "-request_count": "request_count DESC",
}

_LIST_DETAILS = """
SELECT id, sandbox_id, uri, method, target, request_time, received_at
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
    ) -> dict:
        """List per-sandbox latest requests, sorted by ``sort``.

        ``search`` filters sandbox ids by substring (case-insensitive,
        fuzzy). ``sort`` is a whitelisted key from ``_LATEST_ORDERS``
        (``-`` prefix means descending); default is newest first.
        """
        try:
            order = _LATEST_ORDERS[sort]
        except KeyError:
            raise ValueError(f"invalid sort key: {sort}") from None

        params: dict = {"limit": limit, "offset": offset}
        if search:
            # Fuzzy match on sandbox_id; escape LIKE wildcards in the input
            # so user input is matched literally.
            params["pattern"] = f"%{_like_escape(search)}%"
            where = "WHERE sandbox_id ILIKE %(pattern)s ESCAPE '\\'"
        else:
            where = ""

        with self.pool.connection() as conn:
            conn.row_factory = dict_row
            with conn.cursor() as cur:
                cur.execute(_LIST_LATEST.format(where=where, order=order), params)
                rows = cur.fetchall()
                cur.execute(_COUNT_LATEST.format(where=where), params)
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
                cur.execute(_COUNT_DETAILS.format(where=where), params)
                total = cur.fetchone()["total"]
        return {"total": total, "items": [_jsonify(row) for row in rows]}


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


def _like_escape(text: str) -> str:
    """Escape LIKE/ILIKE wildcards so the search input matches literally."""
    return (
        text.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    )
