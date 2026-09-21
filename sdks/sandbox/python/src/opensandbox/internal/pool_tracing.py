# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
"""Best-effort OpenTelemetry spans for client-pool warmup."""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from types import TracebackType
from typing import Any

from opentelemetry import trace
from opentelemetry.trace import Span, Status, StatusCode

WARMUP_ROOT_SPAN = "pool.warmup"
WARMUP_CREATE_SPAN = "pool.warmup.create"
WARMUP_READINESS_CHECK_SPAN = "pool.warmup.readiness"
WARMUP_PREPARE_SPAN = "pool.warmup.prepare"
WARMUP_POST_PREPARE_CHECK_SPAN = "pool.warmup.post_prepare_readiness"
WARMUP_RENEW_SPAN = "pool.warmup.renew"
WARMUP_COMMIT_SPAN = "pool.warmup.commit"

_INSTRUMENTATION_NAME = "opensandbox"


class PoolTracer:
    """Creates no spans unless tracing is explicitly enabled."""

    def __init__(self, enabled: bool) -> None:
        self._tracer = trace.get_tracer(_INSTRUMENTATION_NAME) if enabled else None

    def start_warmup(
        self,
        *,
        pool_name: str,
        owner_id: str,
        run_generation: int,
        leader_epoch: int,
        submitted_ns: int,
        image: str,
    ) -> WarmupTrace:
        if self._tracer is None:
            return WarmupTrace(None)
        try:
            span = self._tracer.start_span(
                WARMUP_ROOT_SPAN,
                start_time=submitted_ns,
                attributes={
                    "pool.name": pool_name,
                    "pool.owner": owner_id,
                    "pool.run.generation": run_generation,
                    "pool.leader.epoch": leader_epoch,
                    "sandbox.image": image,
                },
            )
            return WarmupTrace(span)
        except Exception:
            return WarmupTrace(None)


class WarmupTrace:
    def __init__(self, root: Span | None) -> None:
        self._root = root

    @contextmanager
    def phase(self, name: str) -> Iterator[Span | None]:
        root = self._root
        if root is None:
            yield None
            return
        try:
            with trace.use_span(root, end_on_exit=False):
                with trace.get_tracer(_INSTRUMENTATION_NAME).start_as_current_span(
                    name
                ) as span:
                    try:
                        yield span
                        span.set_attribute("warmup.result", "success")
                    except BaseException as exc:
                        span.set_attribute("warmup.result", "failure")
                        span.set_attribute("warmup.error.type", type(exc).__qualname__)
                        span.set_status(Status(StatusCode.ERROR))
                        span.record_exception(exc)
                        raise
        except BaseException:
            raise

    def set_sandbox_id(self, sandbox_id: str) -> None:
        self._set_attribute("sandbox.id", sandbox_id)

    def end_success(self) -> None:
        self._finish(stage="commit", result="success")

    def end_dropped(self, stage: str, reason: str) -> None:
        self._finish(stage=stage, result="dropped", reason=reason, error=True)

    def end_cancelled(self, stage: str) -> None:
        self._finish(stage=stage, result="cancelled", reason="cancelled", error=True)

    def end_failure(self, stage: str, error: BaseException) -> None:
        root = self._root
        if root is None:
            return
        try:
            root.set_attribute("warmup.error.type", type(error).__qualname__)
            root.set_attribute("warmup.error.category", _classify_error(error, stage))
            root.record_exception(error)
            self._finish(
                stage=stage,
                result="failure",
                reason=f"{stage}_failed",
                error=True,
            )
        except Exception:
            self._safe_end()

    def _finish(
        self,
        *,
        stage: str,
        result: str,
        reason: str | None = None,
        error: bool = False,
    ) -> None:
        root = self._root
        if root is None:
            return
        try:
            root.set_attribute("warmup.stage", stage)
            root.set_attribute("warmup.result", result)
            if reason is not None:
                root.set_attribute("warmup.reason", reason)
            if error:
                root.set_status(Status(StatusCode.ERROR))
        finally:
            self._safe_end()

    def _set_attribute(self, key: str, value: Any) -> None:
        if self._root is None:
            return
        try:
            self._root.set_attribute(key, value)
        except Exception:
            pass

    def _safe_end(self) -> None:
        root, self._root = self._root, None
        if root is not None:
            try:
                root.end()
            except Exception:
                pass

    def __enter__(self) -> WarmupTrace:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        if exc is not None:
            self.end_failure("unknown", exc)
        elif self._root is not None:
            self.end_dropped("unknown", "unfinished")


def annotate_health_span(
    span: Span | None,
    *,
    attempt_count: int,
    false_count: int,
    exception_count: int,
) -> None:
    if span is None:
        return
    try:
        span.set_attribute("warmup.health.attempt_count", attempt_count)
        span.set_attribute("warmup.health.false_count", false_count)
        span.set_attribute("warmup.health.exception_count", exception_count)
    except Exception:
        pass


def _classify_error(error: BaseException, stage: str) -> str:
    name = type(error).__qualname__.lower()
    module = type(error).__module__.lower()
    if "timeout" in name:
        return "timeout"
    if "connect" in name or "network" in name or module.startswith("httpx"):
        return "connection"
    if "state" in name or stage == "commit":
        return "state_store"
    if stage in {"readiness", "prepare", "post_prepare_readiness"}:
        return "callback"
    return "unclassified"
