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

"""OpenTelemetry metrics for Server: HTTP, sandbox lifecycle, snapshots, proxy."""

from __future__ import annotations

import asyncio
import logging
from contextlib import contextmanager
from dataclasses import dataclass
from functools import wraps
from inspect import iscoroutinefunction
from time import perf_counter
from typing import TYPE_CHECKING, Optional

from fastapi import HTTPException
from opentelemetry import metrics
from opentelemetry.metrics import CallbackOptions, Observation
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.metrics.view import ExplicitBucketHistogramAggregation, View
from opentelemetry.sdk.resources import Resource

if TYPE_CHECKING:
    from opentelemetry.metrics import Counter, Histogram

    from opensandbox_server.config import OtelConfig

logger = logging.getLogger(__name__)

_METER_NAME = "opensandbox.server"

_CREATE_DURATION_HISTOGRAM_NAME = "opensandbox.sandbox.create.duration"
_CREATE_DURATION_UNIT = "ms"
_CREATE_DURATION_DESCRIPTION = (
    "Sandbox creation latency from SDK create start until ready or failure"
)
_HTTP_REQUEST_DURATION_HISTOGRAM_NAME = "server.http.request.duration"
_HTTP_REQUEST_DURATION_UNIT = "ms"
_HTTP_REQUEST_DURATION_DESCRIPTION = (
    "Server HTTP request duration by method, route template, and status code"
)
_HTTP_REQUEST_METHODS = frozenset(
    {"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"}
)

_HTTP_REQUEST_DURATION_BOUNDARIES = (
    1.0, 5.0, 10.0, 25.0, 50.0, 100.0, 250.0, 500.0, 1000.0,
    2500.0, 5000.0, 10000.0, 30000.0, 60000.0,
)
# Lifecycle durations (create, delete, snapshot) run past 60s on cold Kubernetes
# starts, so the ladder gets a longer tail than the HTTP one.
_LIFECYCLE_DURATION_BOUNDARIES = (
    100.0, 250.0, 500.0, 1000.0, 2500.0, 5000.0, 10000.0,
    30000.0, 60000.0, 120000.0, 300000.0,
)

_LIFECYCLE_OPERATIONS = ("create", "delete", "pause", "resume", "renew", "orphan_cleaned")
_LIFECYCLE_DURATION_OPERATIONS = frozenset({"create", "delete"})
_SNAPSHOT_OPERATIONS = ("create", "delete")
_ACTIVE_SANDBOX_STATES = frozenset(
    {"pending", "running", "pausing", "paused", "resuming", "stopping", "terminated", "failed"}
)

_meter_provider: Optional[MeterProvider] = None
_create_duration_histogram = None
_http_request_duration_histogram = None
_business_instruments: Optional[_BusinessInstruments] = None


@dataclass
class _BusinessInstruments:
    lifecycle_counters: dict
    lifecycle_durations: dict
    snapshot_counters: dict
    snapshot_durations: dict
    proxy_request_counter: "Counter"
    proxy_request_duration: "Histogram"
    access_renew_counter: "Counter"


def _histogram_from_provider(provider: MeterProvider):
    return provider.get_meter(_METER_NAME).create_histogram(
        name=_CREATE_DURATION_HISTOGRAM_NAME,
        unit=_CREATE_DURATION_UNIT,
        description=_CREATE_DURATION_DESCRIPTION,
    )


def _http_request_histogram_from_provider(provider: MeterProvider):
    return provider.get_meter(_METER_NAME).create_histogram(
        name=_HTTP_REQUEST_DURATION_HISTOGRAM_NAME,
        unit=_HTTP_REQUEST_DURATION_UNIT,
        description=_HTTP_REQUEST_DURATION_DESCRIPTION,
    )


def _business_instruments_from_provider(provider: MeterProvider) -> _BusinessInstruments:
    meter = provider.get_meter(_METER_NAME)

    lifecycle_counters = {
        operation: meter.create_counter(
            name=f"server.sandbox.{operation}.count",
            description=f"Sandbox {operation.replace('_', ' ')} operations",
        )
        for operation in _LIFECYCLE_OPERATIONS
    }
    lifecycle_durations = {
        operation: meter.create_histogram(
            name=f"server.sandbox.{operation}.duration",
            unit="ms",
            description=f"Server-side sandbox {operation} duration",
        )
        for operation in _LIFECYCLE_OPERATIONS
        if operation in _LIFECYCLE_DURATION_OPERATIONS
    }
    snapshot_counters = {
        operation: meter.create_counter(
            name=f"server.snapshot.{operation}.count",
            description=f"Snapshot {operation} operations, counted at terminal state",
        )
        for operation in _SNAPSHOT_OPERATIONS
    }
    snapshot_durations = {
        "create": meter.create_histogram(
            name="server.snapshot.create.duration",
            unit="ms",
            description="Server-side snapshot creation duration",
        )
    }
    return _BusinessInstruments(
        lifecycle_counters=lifecycle_counters,
        lifecycle_durations=lifecycle_durations,
        snapshot_counters=snapshot_counters,
        snapshot_durations=snapshot_durations,
        proxy_request_counter=meter.create_counter(
            name="server.proxy.request.count",
            description="Server-proxy requests to sandbox endpoints",
        ),
        proxy_request_duration=meter.create_histogram(
            name="server.proxy.request.duration",
            unit="ms",
            description="Server-proxy request duration; websockets cover the whole session",
        ),
        access_renew_counter=meter.create_counter(
            name="server.access_renew.outcome.count",
            description="Access-renew pipeline outcomes",
        ),
    )


def _configured_runtime_label() -> str:
    from opensandbox_server.config import get_config

    return get_config().runtime.type.lower()


def _runtime_label_for_sandbox_id(sandbox_id: str) -> str:
    runtime_type = _configured_runtime_label()
    if runtime_type == "kubernetes":
        return "fleets" if sandbox_id.startswith("flt-") else "kubernetes"
    return runtime_type


def _tenant_entries() -> list:
    """Tenant entries for cross-namespace gauge aggregation; empty when N/A."""
    try:
        from opensandbox_server.main import tenant_provider
    except Exception:
        return []
    if tenant_provider is None or not getattr(tenant_provider, "supports_enumeration", False):
        return []
    try:
        return tenant_provider.list_tenants()
    except Exception:
        logger.warning("Failed to list tenants for server.sandbox.active", exc_info=True)
        return []


def _sandbox_active_observations() -> list:
    from opensandbox_server.api.lifecycle import sandbox_service
    from opensandbox_server.api.schema import (
        ListSandboxesRequest,
        PaginationRequest,
        SandboxFilter,
    )
    from opensandbox_server.tenants.context import set_current_tenant

    counts: dict = {}
    seen_ids: set = set()

    def _collect() -> None:
        page = 1
        while page <= 100:
            response = sandbox_service.list_sandboxes(
                ListSandboxesRequest(
                    filter=SandboxFilter(state=None, metadata=None),
                    pagination=PaginationRequest(page=page, pageSize=200),
                )
            )
            for item in response.items or []:
                if item.id in seen_ids:
                    continue
                state = (item.status.state or "").lower()
                if state not in _ACTIVE_SANDBOX_STATES:
                    continue
                seen_ids.add(item.id)
                key = (_runtime_label_for_sandbox_id(item.id), state)
                counts[key] = counts.get(key, 0) + 1
            if not (response.pagination and response.pagination.has_next_page):
                break
            page += 1

    # Default context (default namespace / docker), then each tenant namespace:
    # metric collection runs without a request tenant, so multi-tenant
    # deployments need an explicit per-namespace sweep. Sandbox IDs dedupe
    # overlaps between the default namespace and tenant entries.
    _collect()
    for entry in _tenant_entries():
        if not getattr(entry, "namespace", None):
            continue
        set_current_tenant(entry)
        try:
            _collect()
        finally:
            set_current_tenant(None)

    return [
        Observation(count, {"state": state, "runtime": runtime})
        for (runtime, state), count in counts.items()
    ]


def _sandbox_active_callback(_options: CallbackOptions) -> list:
    try:
        return _sandbox_active_observations()
    except Exception:
        logger.warning("Failed to observe server.sandbox.active", exc_info=True)
        return []


def _register_sandbox_active_gauge(provider: MeterProvider) -> None:
    provider.get_meter(_METER_NAME).create_observable_gauge(
        name="server.sandbox.active",
        description="Current sandbox count by lifecycle state and runtime",
        callbacks=[_sandbox_active_callback],
    )


def setup_otel_metrics(config: OtelConfig) -> None:
    """Configure OTEL metrics export when enabled; otherwise keep recording as noop."""
    global _meter_provider, _create_duration_histogram, _http_request_duration_histogram
    global _business_instruments

    # Disabled: do not attach instruments to any global provider (may already export).
    if not config.enabled:
        _create_duration_histogram = None
        _http_request_duration_histogram = None
        _business_instruments = None
        logger.info(
            "OpenTelemetry metrics export disabled; Server and SDK metrics are noops"
        )
        return

    try:
        from opentelemetry.exporter.otlp.proto.http.metric_exporter import (
            OTLPMetricExporter,
        )
    except ImportError as exc:  # pragma: no cover
        raise RuntimeError(
            "opentelemetry-exporter-otlp-proto-http is required when [otel] enabled=true"
        ) from exc

    endpoint = (config.endpoint or "").strip() or None
    exporter_kwargs = {}
    if endpoint:
        exporter_kwargs["endpoint"] = endpoint

    exporter = OTLPMetricExporter(**exporter_kwargs)
    reader = PeriodicExportingMetricReader(
        exporter,
        export_interval_millis=config.export_interval_millis,
    )
    resource = Resource.create({"service.name": config.service_name})
    views = []
    for name in (
        _CREATE_DURATION_HISTOGRAM_NAME,
        _HTTP_REQUEST_DURATION_HISTOGRAM_NAME,
        "server.sandbox.create.duration",
        "server.sandbox.delete.duration",
        "server.snapshot.create.duration",
        "server.proxy.request.duration",
    ):
        boundaries = (
            _HTTP_REQUEST_DURATION_BOUNDARIES
            if name in (
                _HTTP_REQUEST_DURATION_HISTOGRAM_NAME,
                "server.proxy.request.duration",
            )
            else _LIFECYCLE_DURATION_BOUNDARIES
        )
        views.append(
            View(
                instrument_name=name,
                aggregation=ExplicitBucketHistogramAggregation(boundaries=list(boundaries)),
            )
        )
    provider = MeterProvider(
        resource=resource,
        metric_readers=[reader],
        views=views,
    )

    current = metrics.get_meter_provider()
    if isinstance(current, MeterProvider):
        logger.warning(
            "A global MeterProvider is already installed; opensandbox will export "
            "metrics via its own provider and will not replace the global one"
        )
    else:
        metrics.set_meter_provider(provider)

    # Always bind instruments to *this* provider so export uses our OTLP reader,
    # even when set_meter_provider() cannot override a preexisting global provider.
    _meter_provider = provider
    _create_duration_histogram = _histogram_from_provider(provider)
    _http_request_duration_histogram = _http_request_histogram_from_provider(provider)
    _business_instruments = _business_instruments_from_provider(provider)
    _register_sandbox_active_gauge(provider)
    logger.info(
        "OpenTelemetry metrics enabled (service=%s, endpoint=%s)",
        config.service_name,
        endpoint or "(default from OTEL_EXPORTER_OTLP_* env)",
    )


def shutdown_otel_metrics() -> None:
    """Flush and shut down the configured MeterProvider if any."""
    global _meter_provider, _create_duration_histogram, _http_request_duration_histogram
    global _business_instruments
    provider = _meter_provider
    _meter_provider = None
    _create_duration_histogram = None
    _http_request_duration_histogram = None
    _business_instruments = None
    if provider is None:
        return
    try:
        provider.shutdown()
    except Exception:
        logger.exception("Failed to shut down OpenTelemetry MeterProvider")


def record_sandbox_create_duration(
    *,
    create_duration_ms: int,
    sdk_language: str,
    sdk_version: str,
    success: bool,
) -> None:
    """Record a sandbox.create duration sample. Never raises.

    No-ops when OTEL export is disabled or setup has not installed a histogram.
    """
    hist = _create_duration_histogram
    if hist is None:
        return
    try:
        hist.record(
            float(create_duration_ms),
            attributes={
                "sdk.language": sdk_language,
                "sdk.version": sdk_version,
                "success": success,
            },
        )
    except Exception:
        logger.exception("Failed to record sandbox create duration metric")


def record_http_request_duration(
    *,
    duration_ms: float,
    method: str,
    route: str,
    status_code: int,
) -> None:
    """Record a low-cardinality Server HTTP duration sample. Never raises."""
    hist = _http_request_duration_histogram
    if hist is None:
        return
    normalized_method = method.upper()
    if normalized_method not in _HTTP_REQUEST_METHODS:
        normalized_method = "OTHER"
    try:
        hist.record(
            duration_ms,
            attributes={
                "http_method": normalized_method,
                "http_route": route or "unknown",
                "http_status_code": status_code,
            },
        )
    except Exception:
        logger.exception("Failed to record Server HTTP request duration metric")


def create_source_label(request) -> str:
    """Bounded source label for a sandbox create request."""
    if getattr(request, "snapshot_id", None):
        return "snapshot"
    if (getattr(request, "extensions", None) or {}).get("poolRef", "").strip():
        return "pool"
    return "image"


def _create_error_type_from_exception(exc: BaseException) -> str:
    if isinstance(exc, HTTPException):
        detail = exc.detail if isinstance(exc.detail, dict) else {}
        code = str(detail.get("code", "")).upper()
        if exc.status_code == 403 or "QUOTA" in code:
            return "quota"
        if exc.status_code == 429:
            return "pool_timeout"
        if exc.status_code in (408, 504) or "TIMEOUT" in code:
            return "timeout"
        if exc.status_code == 404:
            return "image"
        if exc.status_code in (400, 409, 422):
            return "invalid_request"
        return "runtime"
    return "unknown"


def _lifecycle_attrs(
    operation: str,
    runtime: str,
    result: str,
    error_type: Optional[str],
    source: Optional[str],
    trigger: Optional[str],
) -> dict:
    attrs = {"runtime": runtime}
    if operation != "orphan_cleaned":
        attrs["result"] = result
    if operation == "create":
        attrs["error_type"] = error_type or "unknown"
        attrs["source"] = source or "image"
    if operation == "delete":
        attrs["trigger"] = trigger or "user"
    return attrs


def record_lifecycle_operation(
    operation: str,
    *,
    runtime: str,
    result: str = "success",
    duration_ms: Optional[float] = None,
    error_type: Optional[str] = None,
    source: Optional[str] = None,
    trigger: Optional[str] = None,
) -> None:
    """Record a sandbox lifecycle operation. Never raises."""
    instruments = _business_instruments
    if instruments is None:
        return
    try:
        counter = instruments.lifecycle_counters.get(operation)
        if counter is not None:
            counter.add(
                1,
                _lifecycle_attrs(operation, runtime, result, error_type, source, trigger),
            )
        histogram = instruments.lifecycle_durations.get(operation)
        if histogram is not None and duration_ms is not None:
            histogram.record(duration_ms, {"runtime": runtime, "result": result})
    except Exception:
        logger.exception("Failed to record sandbox lifecycle metric")


@contextmanager
def measure_lifecycle_operation(
    operation: str,
    runtime: str,
    *,
    source: Optional[str] = None,
    trigger: Optional[str] = None,
):
    """Time and count a sandbox lifecycle operation, recording success or error."""
    started_at = perf_counter()
    try:
        yield
    except HTTPException as exc:
        record_lifecycle_operation(
            operation,
            runtime=runtime,
            result="error",
            duration_ms=(perf_counter() - started_at) * 1000.0,
            error_type=_create_error_type_from_exception(exc) if operation == "create" else None,
            source=source,
            trigger=trigger,
        )
        raise
    except asyncio.CancelledError:
        # Cancellation bypasses ``except Exception``; record the interrupted
        # call without classifying it, then let cancellation propagate.
        record_lifecycle_operation(
            operation,
            runtime=runtime,
            result="error",
            duration_ms=(perf_counter() - started_at) * 1000.0,
            source=source,
            trigger=trigger,
        )
        raise
    except Exception:
        record_lifecycle_operation(
            operation,
            runtime=runtime,
            result="error",
            duration_ms=(perf_counter() - started_at) * 1000.0,
            error_type="runtime" if operation == "create" else None,
            source=source,
            trigger=trigger,
        )
        raise
    else:
        record_lifecycle_operation(
            operation,
            runtime=runtime,
            result="success",
            duration_ms=(perf_counter() - started_at) * 1000.0,
            source=source,
            trigger=trigger,
        )


def instrument_lifecycle(operation: str):
    """Decorate a sandbox lifecycle service method with business metrics.

    The runtime label is resolved per call: a ``sandbox_id`` string argument
    routes ``flt-*`` ids to fleets under the kubernetes composite, otherwise
    the configured runtime type is used. ``create`` additionally records the
    source (image/snapshot/pool) and a bounded ``error_type``; ``delete``
    records ``trigger=user`` (expiry and recovery paths record explicitly
    where they happen).
    """

    def _runtime_and_source(args: tuple, kwargs: dict) -> tuple:
        sandbox_id = kwargs.get("sandbox_id")
        if not sandbox_id and args and isinstance(args[0], str):
            sandbox_id = args[0]
        runtime = (
            _runtime_label_for_sandbox_id(sandbox_id)
            if sandbox_id
            else _configured_runtime_label()
        )
        source = None
        if operation == "create":
            request = kwargs.get("request") or (args[0] if args else None)
            source = create_source_label(request)
        return runtime, source

    def decorator(func):
        trigger = "user" if operation == "delete" else None

        if iscoroutinefunction(func):
            @wraps(func)
            async def async_wrapper(self, *args, **kwargs):
                runtime, source = _runtime_and_source(args, kwargs)
                with measure_lifecycle_operation(
                    operation, runtime, source=source, trigger=trigger
                ):
                    return await func(self, *args, **kwargs)

            return async_wrapper

        @wraps(func)
        def wrapper(self, *args, **kwargs):
            runtime, source = _runtime_and_source(args, kwargs)
            with measure_lifecycle_operation(
                operation, runtime, source=source, trigger=trigger
            ):
                return func(self, *args, **kwargs)

        return wrapper

    return decorator


def record_snapshot_operation(
    operation: str,
    *,
    runtime: str,
    result: str,
    duration_ms: Optional[float] = None,
    count: bool = True,
) -> None:
    """Record a snapshot operation at terminal state. Never raises.

    ``count=False`` records only the duration sample; the terminal-state
    counter is owned by the repository transition site.
    """
    instruments = _business_instruments
    if instruments is None:
        return
    try:
        if count:
            counter = instruments.snapshot_counters.get(operation)
            if counter is not None:
                counter.add(1, {"runtime": runtime, "result": result})
        if duration_ms is not None:
            histogram = instruments.snapshot_durations.get(operation)
            if histogram is not None:
                histogram.record(duration_ms, {"runtime": runtime, "result": result})
    except Exception:
        logger.exception("Failed to record snapshot metric")


def record_proxy_request(
    *,
    proxy_type: str,
    method: str,
    status_code: int,
    duration_ms: float,
) -> None:
    """Record a server-proxy request. Never raises."""
    instruments = _business_instruments
    if instruments is None:
        return
    normalized_method = method.upper()
    if normalized_method not in _HTTP_REQUEST_METHODS:
        normalized_method = "OTHER"
    attrs = {
        "proxy_type": proxy_type,
        "http_method": normalized_method,
        "http_status_code": status_code,
    }
    try:
        instruments.proxy_request_counter.add(1, attrs)
        instruments.proxy_request_duration.record(duration_ms, attrs)
    except Exception:
        logger.exception("Failed to record proxy request metric")


def record_access_renew_outcome(outcome: str) -> None:
    """Record an access-renew pipeline outcome. Never raises."""
    instruments = _business_instruments
    if instruments is None:
        return
    try:
        instruments.access_renew_counter.add(1, {"outcome": outcome})
    except Exception:
        logger.exception("Failed to record access renew outcome metric")


def instrument_proxy_http(func):
    """Decorate an async server-proxy HTTP handler with request metrics.

    Latency covers up to the proxied response headers (the body streams after);
    ``HTTPException`` status codes are recorded, anything else as 500.
    """

    @wraps(func)
    async def wrapper(request, *args, **kwargs):
        started_at = perf_counter()
        status_code = 500
        try:
            response = await func(request, *args, **kwargs)
            status_code = response.status_code
            return response
        except HTTPException as exc:
            status_code = exc.status_code
            raise
        except asyncio.CancelledError:
            status_code = 499
            raise
        except Exception:
            status_code = 500
            raise
        finally:
            record_proxy_request(
                proxy_type="http",
                method=request.method,
                status_code=status_code,
                duration_ms=(perf_counter() - started_at) * 1000.0,
            )

    return wrapper
