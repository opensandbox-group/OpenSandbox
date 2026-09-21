# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

from __future__ import annotations

import time

from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from opensandbox.internal import pool_tracing


def test_warmup_trace_emits_root_phase_and_health_attributes(monkeypatch) -> None:
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(pool_tracing.trace, "get_tracer", provider.get_tracer)

    tracer = pool_tracing.PoolTracer(enabled=True)
    warmup = tracer.start_warmup(
        pool_name="trace-pool",
        owner_id="trace-owner",
        run_generation=2,
        leader_epoch=3,
        submitted_ns=time.time_ns(),
        image="ubuntu:22.04",
    )
    warmup.set_sandbox_id("sandbox-1")
    with warmup.phase(pool_tracing.WARMUP_READINESS_CHECK_SPAN) as span:
        pool_tracing.annotate_health_span(
            span,
            attempt_count=3,
            false_count=1,
            exception_count=1,
        )
    warmup.end_success()

    spans = {span.name: span for span in exporter.get_finished_spans()}
    root = spans[pool_tracing.WARMUP_ROOT_SPAN]
    readiness = spans[pool_tracing.WARMUP_READINESS_CHECK_SPAN]
    assert readiness.parent is not None
    assert readiness.parent.span_id == root.context.span_id
    assert root.attributes["pool.name"] == "trace-pool"
    assert root.attributes["pool.run.generation"] == 2
    assert root.attributes["pool.leader.epoch"] == 3
    assert root.attributes["sandbox.id"] == "sandbox-1"
    assert root.attributes["warmup.result"] == "success"
    assert root.attributes["warmup.stage"] == "commit"
    assert readiness.attributes["warmup.health.attempt_count"] == 3
    assert readiness.attributes["warmup.health.false_count"] == 1
    assert readiness.attributes["warmup.health.exception_count"] == 1
