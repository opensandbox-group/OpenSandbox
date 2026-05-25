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

"""
FastAPI application entry point for OpenSandbox Lifecycle API.

This module initializes the FastAPI application with middleware, routes,
and configuration for the sandbox lifecycle management service.
"""

import logging
import os
from contextlib import asynccontextmanager
from typing import Any

import httpx
from fastapi import FastAPI, Request
from fastapi.exceptions import HTTPException
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from opensandbox_server.config import load_config
from opensandbox_server.integrations.renew_intent import start_renew_intent_consumer
from opensandbox_server.logging_config import configure_logging
from opensandbox_server.startup_guard import api_key_confirm

# Load configuration before initializing routers/middleware
app_config = load_config()
_log_config = configure_logging(app_config.log)

from opensandbox_server.api.devops import router as devops_router  # noqa: E402
from opensandbox_server.api.pool import router as pool_router  # noqa: E402
from opensandbox_server.api.lifecycle import router, sandbox_service, snapshot_service  # noqa: E402
from opensandbox_server.api.proxy import router as proxy_router  # noqa: E402
from opensandbox_server.integrations.renew_intent.proxy_renew import ProxyRenewCoordinator  # noqa: E402
from opensandbox_server.middleware.auth import AuthMiddleware  # noqa: E402
from opensandbox_server.middleware.request_id import RequestIdMiddleware  # noqa: E402
from opensandbox_server.services.extension_service import require_extension_service  # noqa: E402
from opensandbox_server.services.runtime_resolver import (  # noqa: E402
    validate_secure_runtime_on_startup,
)

logger = logging.getLogger(__name__)

@asynccontextmanager
async def lifespan(app: FastAPI):
    try:
        api_key_confirm(configured_api_key=app_config.server.api_key)
    except Exception as exc:
        logger.error("API key startup confirmation failed: %s", exc)
        os._exit(1)

    from anyio.to_thread import current_default_thread_limiter

    current_default_thread_limiter().total_tokens = app_config.server.thread_pool_size

    app.state.http_client = httpx.AsyncClient(timeout=180.0)

    # Validate secure runtime configuration at startup
    try:
        # Determine which runtime client to create based on config
        docker_client = None
        k8s_client = None
        runtime_type = app_config.runtime.type

        if runtime_type == "docker":
            import docker

            docker_client = docker.from_env()
            logger.info("Validating secure runtime for Docker backend")
        elif runtime_type == "kubernetes":
            from opensandbox_server.services.k8s.client import K8sClient

            k8s_client = K8sClient(app_config.kubernetes)
            logger.info("Validating secure runtime for Kubernetes backend")

        await validate_secure_runtime_on_startup(
            app_config,
            docker_client=docker_client,
            k8s_client=k8s_client,
        )

    except Exception as exc:
        logger.error("Secure runtime validation failed: %s", exc)
        raise

    ext = require_extension_service(sandbox_service)
    app.state.renew_intent_consumer = await start_renew_intent_consumer(
        app_config,
        sandbox_service,
        ext,
    )
    app.state.renew_intent_runner = app.state.renew_intent_consumer

    app.state.proxy_renew_coordinator = ProxyRenewCoordinator(
        app_config,
        app.state.renew_intent_consumer,
    )

    yield

    consumer = getattr(app.state, "renew_intent_consumer", None)
    if consumer is not None:
        await consumer.stop()
    snapshot_service.close()
    await app.state.http_client.aclose()


# Initialize FastAPI application
app = FastAPI(
    title="OpenSandbox Lifecycle API",
    version="0.1.0",
    description="The Sandbox Lifecycle API coordinates how untrusted workloads are created, "
                "executed, paused, resumed, and finally disposed.",
    docs_url="/docs",
    redoc_url="/redoc",
    lifespan=lifespan,
)

# Attach global config for runtime access
app.state.config = app_config

# Middleware run in reverse order of addition: last added = first to run (outermost).
# Add auth and CORS first so they run after RequestIdMiddleware.
app.add_middleware(AuthMiddleware, config=app_config)
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)
# RequestIdMiddleware last = outermost: runs first, so every response (including
# 401 from AuthMiddleware) gets X-Request-ID and logs have request_id in context.
app.add_middleware(RequestIdMiddleware)

# Include API routes at root and versioned prefix.
# IMPORTANT: devops_router and pool_router MUST be registered before proxy_router
# because proxy_router contains catch-all routes that would swallow diagnostics paths.
app.include_router(router)
app.include_router(devops_router)
app.include_router(pool_router)
app.include_router(proxy_router)
app.include_router(router, prefix="/v1")
app.include_router(devops_router, prefix="/v1")
app.include_router(pool_router, prefix="/v1")
app.include_router(proxy_router, prefix="/v1")

DEFAULT_ERROR_CODE = "GENERAL::UNKNOWN_ERROR"
DEFAULT_ERROR_MESSAGE = "An unexpected error occurred."


def _normalize_error_detail(detail: Any) -> dict[str, str]:
    """
    Ensure HTTP errors always conform to {"code": "...", "message": "..."}.
    """
    if isinstance(detail, dict):
        code = detail.get("code") or DEFAULT_ERROR_CODE
        message = detail.get("message") or DEFAULT_ERROR_MESSAGE
        return {"code": code, "message": message}
    message = str(detail) if detail else DEFAULT_ERROR_MESSAGE
    return {"code": DEFAULT_ERROR_CODE, "message": message}


@app.exception_handler(HTTPException)
async def sandbox_http_exception_handler(request: Request, exc: HTTPException):
    """
    Flatten FastAPI HTTPException payload to the standard error schema.
    """
    content = _normalize_error_detail(exc.detail)
    return JSONResponse(
        status_code=exc.status_code,
        content=content,
        headers=exc.headers,
    )


@app.get("/health")
async def health_check():
    """
    Health check endpoint.

    Returns:
        dict: Health status
    """
    return {"status": "healthy"}


if __name__ == "__main__":
    import uvicorn

    # Run the application
    uvicorn.run(
        "opensandbox_server.main:app",
        host=app_config.server.host,
        port=app_config.server.port,
        reload=True,
        log_config=_log_config,
        timeout_keep_alive=app_config.server.timeout_keep_alive,
        loop=app_config.server.loop,
        http=app_config.server.http,
    )
