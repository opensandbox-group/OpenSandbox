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

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.config import get_settings
from app.lifecycle import LifecycleClient
from app.history.store import close_history_pool, init_history_pool
from app.routes import admin, auth, history, pools, sandboxes, snapshots

app = FastAPI(title="OpenSandbox Console BFF", version="0.1.0")


@app.on_event("startup")
def validate_config() -> None:
    settings = get_settings()
    if not settings.tenants_toml_path:
        raise RuntimeError("TENANTS_TOML_PATH is required")
    if not settings.bff_session_secret or not settings.bff_admin_token:
        raise RuntimeError("BFF_SESSION_SECRET and BFF_ADMIN_TOKEN are required")
    init_history_pool(settings)


@app.on_event("shutdown")
def shutdown_history() -> None:
    close_history_pool()


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/api/version")
async def version() -> dict:
    client = LifecycleClient(get_settings())
    return await client.get_version()


settings = get_settings()
app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.cors_origins_list,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

api = FastAPI()
api.include_router(auth.router)
api.include_router(sandboxes.router)
api.include_router(snapshots.router)
api.include_router(pools.router)
api.include_router(admin.router)
api.include_router(history.router)
app.mount("/api", api)
