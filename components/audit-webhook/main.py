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

"""Sandbox access audit webhook.

Receives audit events POSTed by the OpenSandbox ingress
(`--audit-enabled --audit-webhook-url ...`) and records them to
PostgreSQL: a detail row per request plus a per-sandbox summary row
holding the latest request.

A periodic Kubernetes sync (``kubernetes.sync_interval``) also discovers
BatchSandbox resources whose sandbox id has no database row yet and
inserts them as never-accessed rows, visible in the UI with an ``未访问``
marker.

Web UI (password protected when ``server.ui_password`` is set):
- ``GET /``          - per-sandbox latest requests (summary page)
- ``GET /details``   - request details page, filterable by sandbox id
- ``GET /login``     - login page

Run:
    cp audit.toml.example audit.toml   # then edit settings
    python main.py
"""

import asyncio
import hashlib
import hmac
import logging
import secrets
import time
from contextlib import asynccontextmanager
from datetime import datetime
from pathlib import Path
from typing import Annotated, Union

from fastapi import Depends, FastAPI, HTTPException, Query, Request
from fastapi.responses import FileResponse, HTMLResponse, JSONResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from psycopg_pool import ConnectionPool
from pydantic import BaseModel, Field

import k8s
from config import load_config
from store import AuditStore

# Settings come from audit.toml (see audit.toml.example); the config file
# location can be overridden with the AUDIT_CONFIG_PATH env var.
_config = load_config()

logger = logging.getLogger("audit-webhook")
logging.basicConfig(
    level=_config["log_level"].upper(),
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)

DATABASE_URL = _config["database_url"]
DB_POOL_MIN = _config["db_pool_min"]
DB_POOL_MAX = _config["db_pool_max"]
HOST = _config["host"]
PORT = _config["port"]

# When non-empty, the web UI and query APIs require login with this password.
# Event ingestion (POST /events) is never password protected - the ingress
# must be able to deliver events without a session.
UI_PASSWORD = _config["ui_password"]

# Kubernetes liveness sync: kubeconfig file path (empty = default kubeconfig,
# falling back to in-cluster credentials) and the namespace whose
# batchsandboxes.sandbox.opensandbox.io resources mark live sandbox ids
# (the resource name is the sandbox id). sync_interval (seconds) enables a
# periodic background sync; 0 means sync only via POST /api/sync-deleted.
# Each sync discovers sandboxes that exist in the cluster but not in the
# database (inserted as never-accessed rows), refreshes each sandbox
# pod's node IP, and reconciles the deleted flags.
KUBECONFIG_PATH = _config["kubeconfig"]
K8S_NAMESPACE = _config["k8s_namespace"]
K8S_SYNC_INTERVAL = _config["k8s_sync_interval"]

_SESSION_COOKIE = "audit_session"
_SESSION_TTL = 7 * 24 * 3600  # 7 days, in seconds

_STATIC_DIR = Path(__file__).resolve().parent / "static"

# Matches the ingress AuditEvent JSON payload (pkg/proxy/audit.go).
class AuditEvent(BaseModel):
    sandbox_id: str = Field(min_length=1)
    uri: str
    method: str
    target: str
    request_time: datetime


class AuditEventAccepted(BaseModel):
    accepted: int


class LoginRequest(BaseModel):
    password: str


@asynccontextmanager
async def lifespan(_: FastAPI):
    pool = ConnectionPool(
        conninfo=DATABASE_URL,
        min_size=DB_POOL_MIN,
        max_size=DB_POOL_MAX,
        open=True,
    )
    store = AuditStore(pool)
    try:
        store.init_schema()
    except Exception:
        logger.exception("failed to initialize audit schema")
        pool.close()
        raise

    app.state.store = store
    app.state.k8s_client = k8s.K8sClient(KUBECONFIG_PATH)

    sync_task = None
    if K8S_SYNC_INTERVAL > 0:
        if K8S_NAMESPACE:
            sync_task = asyncio.create_task(_periodic_sync(store))
        else:
            logger.warning(
                "kubernetes.sync_interval is set but kubernetes.namespace is not; "
                "periodic deleted-sync disabled"
            )

    logger.info("audit webhook ready on %s:%s (ui auth %s)", HOST, PORT, "on" if UI_PASSWORD else "off")
    try:
        yield
    finally:
        if sync_task is not None:
            sync_task.cancel()
        pool.close()
        logger.info("audit webhook stopped")


async def _periodic_sync(store: AuditStore) -> None:
    """Sync sandboxes against the cluster on a fixed interval; failures only log.

    The sync does blocking k8s/DB calls (each potentially a slow network
    round trip) - run them in a worker thread so the event loop keeps
    serving requests while a sync is in flight.
    """
    while True:
        try:
            result = await asyncio.to_thread(
                _sync_sandboxes, store, app.state.k8s_client
            )
            logger.info("periodic sync: %s", result)
        except Exception:
            logger.exception("periodic sandbox sync failed")
        await asyncio.sleep(K8S_SYNC_INTERVAL)


def _sync_sandboxes(store: AuditStore, k8s_client: k8s.K8sClient) -> dict:
    """Discover unaccessed sandboxes, refresh node IPs, and reconcile the
    ``deleted`` flags.

    The BatchSandbox resource name is the sandbox id. Sandbox ids that
    exist in the cluster but have no summary row are inserted as
    never-accessed rows carrying the resource's creationTimestamp
    (shown in the UI with an ``未访问`` marker); existing rows missing a
    creation timestamp get it backfilled. The ``node_ip`` of every
    sandbox pod's host is refreshed. Rows whose sandbox id is gone from
    the cluster are flagged deleted.
    """
    sandboxes = k8s_client.list_batch_sandboxes(K8S_NAMESPACE)
    discovered = store.upsert_discovered_sandboxes(sandboxes)
    node_updated = store.update_node_ips(
        k8s_client.list_sandbox_pod_nodes(K8S_NAMESPACE)
    )
    names = [sandbox["name"] for sandbox in sandboxes]
    result = store.sync_deleted_flags(names)
    return {
        "live": len(sandboxes),
        "node_updated": node_updated,
        **discovered,
        **result,
    }


app = FastAPI(
    title="OpenSandbox Audit Webhook",
    description="Records sandbox access audit events from the ingress to PostgreSQL.",
    version="0.1.0",
    lifespan=lifespan,
)

app.mount("/static", StaticFiles(directory=_STATIC_DIR), name="static")


# ---------------------------------------------------------------------------
# Authentication
# ---------------------------------------------------------------------------

def _session_secret() -> bytes:
    """Derive the signing key from the password (sessions reset on change)."""
    return hashlib.sha256(("audit-webhook:" + UI_PASSWORD).encode()).digest()


def _sign(expires: int) -> str:
    return hmac.new(_session_secret(), str(expires).encode(), hashlib.sha256).hexdigest()


def _create_session_token() -> str:
    expires = int(time.time()) + _SESSION_TTL
    return f"{expires}.{_sign(expires)}"


def _valid_session(token: str | None) -> bool:
    if UI_PASSWORD == "":
        return True  # auth disabled
    if not token:
        return False
    expires, _, signature = token.partition(".")
    if not signature or not expires.isdigit():
        return False
    if int(expires) < time.time():
        return False
    return hmac.compare_digest(signature, _sign(int(expires)))


def _session_token(request: Request) -> str | None:
    return request.cookies.get(_SESSION_COOKIE)


def _login_redirect() -> RedirectResponse:
    return RedirectResponse("/login", status_code=303)


@app.get("/login", response_class=HTMLResponse, include_in_schema=False)
def login_page() -> FileResponse:
    return FileResponse(_STATIC_DIR / "login.html")


@app.post("/login")
def login(body: LoginRequest, request: Request) -> JSONResponse:
    """Verify the password and issue a session cookie."""
    if UI_PASSWORD == "":
        return JSONResponse({"ok": True})  # auth disabled: nothing to do
    if not secrets.compare_digest(body.password.encode(), UI_PASSWORD.encode()):
        raise HTTPException(status_code=401, detail="密码错误")
    response = JSONResponse({"ok": True})
    response.set_cookie(
        _SESSION_COOKIE,
        _create_session_token(),
        max_age=_SESSION_TTL,
        httponly=True,
        samesite="lax",
    )
    return response


@app.post("/logout")
def logout() -> JSONResponse:
    response = JSONResponse({"ok": True})
    response.delete_cookie(_SESSION_COOKIE)
    return response


# ---------------------------------------------------------------------------
# Event ingestion (ingress -> webhook, no auth)
# ---------------------------------------------------------------------------

def get_store(request: Request) -> AuditStore:
    store: AuditStore | None = getattr(request.app.state, "store", None)
    if store is None:
        raise HTTPException(status_code=503, detail="audit store not ready")
    return store


StoreDep = Annotated[AuditStore, Depends(get_store)]


@app.post("/events", response_model=AuditEventAccepted)
@app.post("/", response_model=AuditEventAccepted, include_in_schema=False)
def record_events(
    events: Union[AuditEvent, list[AuditEvent]],
    store: StoreDep,
) -> AuditEventAccepted:
    """Accept a single audit event or a batch of events.

    ``POST /`` is accepted as an alias for ``POST /events`` so a webhook
    URL configured without the path still works.

    Requests whose URI ends with "ping" (case-insensitive, e.g.
    `/<sandbox-id>/<port>/ping` health checks) are dropped and not
    recorded to the database.
    """
    if not isinstance(events, list):
        events = [events]
    events = [event for event in events if _should_record(event)]
    if not events:
        return AuditEventAccepted(accepted=0)
    try:
        accepted = store.record([event.model_dump() for event in events])
    except Exception:
        logger.exception("failed to record audit events")
        raise HTTPException(status_code=502, detail="failed to record audit events")
    return AuditEventAccepted(accepted=accepted)


# Requests whose URI ends with this suffix (case-insensitive) are not
# recorded - typically liveness/health-check pings, e.g.
# /610c205a-272e-425f-85bf-b27cae2d9ee3/44772/ping
_EXCLUDED_URI_SUFFIX = "ping"


def _should_record(event: AuditEvent) -> bool:
    return not event.uri.lower().endswith(_EXCLUDED_URI_SUFFIX)


@app.get("/status.ok")
def healthz() -> dict:
    return {"status": "ok"}


# ---------------------------------------------------------------------------
# Query APIs (auth required when server.ui_password is set)
# ---------------------------------------------------------------------------

@app.get("/api/sandboxes")
def list_sandboxes(
    request: Request,
    store: StoreDep,
    search: Annotated[str | None, Query(min_length=1)] = None,
    sort: Annotated[
        str, Query(pattern=r"^-?(request_time|request_count|accessed|created_at)$")
    ] = "-request_time",
    limit: Annotated[int, Query(ge=1, le=500)] = 50,
    offset: Annotated[int, Query(ge=0)] = 0,
    time_from: Annotated[datetime | None, Query()] = None,
    time_to: Annotated[datetime | None, Query()] = None,
) -> dict:
    """List per-sandbox latest requests.

    ``search`` matches sandbox ids by substring (case-insensitive,
    fuzzy) OR node IPs exactly - typing an IP returns every sandbox on
    that node. ``sort`` is ``request_time``, ``request_count``,
    ``created_at`` or ``accessed``; prefix with ``-`` for descending
    (default: newest first; sorting by ``accessed`` ascending puts
    never-accessed sandboxes first). ``time_from``/``time_to`` bound the
    latest request time (ISO 8601, inclusive; naive values are assumed
    to be UTC).
    """
    _require_api_auth(request)
    return _safe_query(
        lambda: store.list_latest(
            search=search,
            sort=sort,
            limit=limit,
            offset=offset,
            time_from=time_from,
            time_to=time_to,
        )
    )


@app.get("/api/requests")
def list_requests(
    request: Request,
    store: StoreDep,
    sandbox_id: Annotated[str | None, Query(min_length=1)] = None,
    limit: Annotated[int, Query(ge=1, le=500)] = 50,
    offset: Annotated[int, Query(ge=0)] = 0,
) -> dict:
    """List access request details, newest first, optionally filtered by sandbox."""
    _require_api_auth(request)
    return _safe_query(
        lambda: store.list_details(sandbox_id=sandbox_id, limit=limit, offset=offset)
    )


@app.post("/api/sync-deleted")
def sync_deleted(request: Request, store: StoreDep) -> dict:
    """Sync the summary table against the cluster.

    Sandboxes whose BatchSandbox resource exists in the namespace
    (``kubernetes.namespace``, via the kubeconfig at
    ``kubernetes.kubeconfig``) but has no database row are inserted as
    never-accessed rows (``accessed = FALSE``, ``created_at`` = the
    resource's creationTimestamp, shown in the UI with an ``未访问``
    marker); existing rows missing a creation timestamp get it
    backfilled. Each sandbox pod's node IP is refreshed into
    ``node_ip``. Then the ``deleted`` flags are reconciled: summary rows
    whose sandbox id is not among the live resource names (the resource
    name is the sandbox id) are marked ``deleted`` and hidden from the
    UI/API; previously deleted ids that reappear are restored.
    """
    _require_api_auth(request)
    if not K8S_NAMESPACE:
        raise HTTPException(
            status_code=400,
            detail="kubernetes.namespace is not configured",
        )
    k8s_client: k8s.K8sClient = getattr(
        request.app.state, "k8s_client", None
    ) or k8s.K8sClient(KUBECONFIG_PATH)
    try:
        result = _sync_sandboxes(store, k8s_client)
    except k8s.K8sError as exc:
        logger.error("k8s sandbox sync failed: %s", exc)
        raise HTTPException(status_code=502, detail=str(exc)) from None
    except Exception:
        logger.exception("failed to sync sandboxes")
        raise HTTPException(status_code=502, detail="failed to sync sandboxes") from None
    return {"namespace": K8S_NAMESPACE, **result}


def _require_api_auth(request: Request) -> None:
    if UI_PASSWORD and not _valid_session(_session_token(request)):
        raise HTTPException(status_code=401, detail="login required")


def _safe_query(query) -> dict:
    try:
        return query()
    except Exception:
        logger.exception("failed to query audit records")
        raise HTTPException(status_code=502, detail="failed to query audit records")


# ---------------------------------------------------------------------------
# Web pages (auth required when server.ui_password is set)
# ---------------------------------------------------------------------------

@app.get("/", response_class=HTMLResponse, include_in_schema=False)
def index(request: Request):
    """Serve the per-sandbox summary page."""
    if UI_PASSWORD and not _valid_session(_session_token(request)):
        return _login_redirect()
    return FileResponse(_STATIC_DIR / "index.html")


@app.get("/details", response_class=HTMLResponse, include_in_schema=False)
def details(request: Request):
    """Serve the request details page."""
    if UI_PASSWORD and not _valid_session(_session_token(request)):
        return _login_redirect()
    return FileResponse(_STATIC_DIR / "details.html")


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host=HOST, port=PORT)
