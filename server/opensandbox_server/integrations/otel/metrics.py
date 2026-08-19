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

"""OpenTelemetry metrics helpers for SDK lifecycle telemetry."""

from __future__ import annotations

import logging
import re
from typing import TYPE_CHECKING, Optional

from opentelemetry import metrics
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.metrics.view import ExplicitBucketHistogramAggregation, View
from opentelemetry.sdk.resources import Resource

if TYPE_CHECKING:
    from opensandbox_server.config import OtelConfig

logger = logging.getLogger(__name__)

_CREATE_DURATION_HISTOGRAM_NAME = "opensandbox.sandbox.create.duration"
_CREATE_DURATION_UNIT = "ms"
_CREATE_DURATION_DESCRIPTION = (
    "Sandbox creation latency from SDK create start until ready or failure"
)
_OPERATION_DURATION_HISTOGRAM_NAME = "opensandbox.sandbox.operation.duration"
_OPERATION_COUNTER_NAME = "opensandbox.sandbox.operation.total"
_OPERATION_DURATION_UNIT = "ms"
_OPERATION_DURATION_DESCRIPTION = (
    "Server-side latency of a sandbox lifecycle operation, measured at the API boundary"
)
_OPERATION_COUNTER_DESCRIPTION = (
    "Sandbox lifecycle operations by outcome, with the server error code when they fail"
)
# An error code is a constant in the source, never interpolated user input. Validating the
# shape anyway keeps the attribute's cardinality bounded no matter what a future call site
# puts in an HTTPException detail.
_ERROR_CODE_PATTERN = re.compile(r"^[A-Z][A-Z0-9_:]{0,63}$")
_ERROR_CODE_FALLBACK = "OTHER"

_CREATE_DURATION_BOUNDARIES = (
    100.0,
    250.0,
    500.0,
    1000.0,
    2500.0,
    5000.0,
    10000.0,
    30000.0,
    60000.0,
)

_meter_provider: Optional[MeterProvider] = None
_create_duration_histogram = None
_operation_duration_histogram = None
_operation_counter = None


def _histogram_from_provider(provider: MeterProvider):
    return provider.get_meter("opensandbox.server").create_histogram(
        name=_CREATE_DURATION_HISTOGRAM_NAME,
        unit=_CREATE_DURATION_UNIT,
        description=_CREATE_DURATION_DESCRIPTION,
    )


def _operation_instruments_from_provider(provider: MeterProvider):
    meter = provider.get_meter("opensandbox.server")
    histogram = meter.create_histogram(
        name=_OPERATION_DURATION_HISTOGRAM_NAME,
        unit=_OPERATION_DURATION_UNIT,
        description=_OPERATION_DURATION_DESCRIPTION,
    )
    counter = meter.create_counter(
        name=_OPERATION_COUNTER_NAME,
        description=_OPERATION_COUNTER_DESCRIPTION,
    )
    return histogram, counter


def setup_otel_metrics(config: OtelConfig) -> None:
    """Configure OTEL metrics export when enabled; otherwise keep recording as noop."""
    global _meter_provider, _create_duration_histogram
    global _operation_duration_histogram, _operation_counter

    # Disabled: do not attach instruments to any global provider (may already export).
    if not config.enabled:
        _create_duration_histogram = None
        _operation_duration_histogram = None
        _operation_counter = None
        logger.info(
            "OpenTelemetry metrics export disabled; SDK events are accepted but not exported"
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
    views = [
        View(
            instrument_name=_CREATE_DURATION_HISTOGRAM_NAME,
            aggregation=ExplicitBucketHistogramAggregation(
                boundaries=list(_CREATE_DURATION_BOUNDARIES)
            ),
        ),
        # Same ladder: both measure a lifecycle operation in milliseconds, and the SDK
        # default boundaries top out at 10s, which is short for a sandbox operation.
        View(
            instrument_name=_OPERATION_DURATION_HISTOGRAM_NAME,
            aggregation=ExplicitBucketHistogramAggregation(
                boundaries=list(_CREATE_DURATION_BOUNDARIES)
            ),
        ),
    ]
    provider = MeterProvider(
        resource=resource,
        metric_readers=[reader],
        views=views,
    )

    current = metrics.get_meter_provider()
    if isinstance(current, MeterProvider):
        logger.warning(
            "A global MeterProvider is already installed; opensandbox will export "
            "create-latency metrics via its own provider and will not replace the global one"
        )
    else:
        metrics.set_meter_provider(provider)

    # Always bind instruments to *this* provider so export uses our OTLP reader,
    # even when set_meter_provider() cannot override a preexisting global provider.
    _meter_provider = provider
    _create_duration_histogram = _histogram_from_provider(provider)
    _operation_duration_histogram, _operation_counter = _operation_instruments_from_provider(provider)
    logger.info(
        "OpenTelemetry metrics enabled (service=%s, endpoint=%s)",
        config.service_name,
        endpoint or "(default from OTEL_EXPORTER_OTLP_* env)",
    )


def shutdown_otel_metrics() -> None:
    """Flush and shut down the configured MeterProvider if any."""
    global _meter_provider, _create_duration_histogram
    global _operation_duration_histogram, _operation_counter
    provider = _meter_provider
    _meter_provider = None
    _create_duration_histogram = None
    _operation_duration_histogram = None
    _operation_counter = None
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


def normalize_error_code(code: object) -> str:
    """Coerce a server error code into a bounded attribute value.

    Codes are constants in the source (``SandboxErrorCodes`` and a few literals), so this
    normally passes them straight through. Anything that is not code-shaped becomes
    ``OTHER``, which keeps the counter's cardinality bounded even if a future call site
    interpolates a message into ``detail["code"]``.
    """
    if isinstance(code, str) and _ERROR_CODE_PATTERN.match(code):
        return code
    return _ERROR_CODE_FALLBACK


def record_sandbox_operation(
    *,
    operation: str,
    duration_ms: float,
    outcome: str,
    error_code: Optional[str] = None,
) -> None:
    """Record a server-side lifecycle operation. Never raises.

    Unlike ``record_sandbox_create_duration``, which stores a number the SDK measured and
    reported, this is the server timing its own work, so it exists for every deployment
    rather than only for those whose clients report metrics.

    ``operation`` is a lifecycle verb from a closed set, ``outcome`` is ``success`` or
    ``error``, and ``error_code`` is attached only on failure.

    No-ops when OTEL export is disabled or setup has not installed the instruments.
    """
    histogram = _operation_duration_histogram
    counter = _operation_counter
    if histogram is None or counter is None:
        return
    attributes = {"operation": operation, "outcome": outcome}
    try:
        histogram.record(float(duration_ms), attributes=attributes)
        if error_code is not None:
            attributes = {**attributes, "error.code": normalize_error_code(error_code)}
        counter.add(1, attributes=attributes)
    except Exception:
        logger.exception("Failed to record sandbox operation metric")
