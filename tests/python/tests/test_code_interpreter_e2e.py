#
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
#
"""
Comprehensive E2E tests for CodeInterpreter functionality.

Tests code execution capabilities including:
- Multi-language code execution (Java, Python, Go, TypeScript)
- Session state management and variable persistence
- Context isolation between different execution contexts
- Error handling and recovery mechanisms
- Event handling patterns identical to runCommand

This file is intentionally split into ordered test methods (rather than one giant test)
to make failures easier to locate and debug.
"""

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable
from contextlib import AsyncExitStack, asynccontextmanager
from datetime import timedelta

import pytest
from code_interpreter import CodeInterpreter
from code_interpreter.models.code import SupportedLanguage
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.constants import DEFAULT_EXECD_PORT
from opensandbox.models.execd import (
    ExecutionComplete,
    ExecutionError,
    ExecutionHandlers,
    ExecutionInit,
    ExecutionResult,
    OutputMessage,
)
from opensandbox.models.sandboxes import Host, SandboxImageSpec, Volume

from tests.base_e2e_test import (
    create_connection_config,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)


def _now_ms() -> int:
    return int(time.time() * 1000)


def _assert_recent_timestamp_ms(ts: int, *, tolerance_ms: int = 180_000) -> None:
    assert isinstance(ts, int)
    assert ts > 0
    delta = abs(_now_ms() - ts)
    assert delta <= tolerance_ms, f"timestamp too far from now: delta={delta}ms (ts={ts})"


def _assert_endpoint_has_port(endpoint: str, expected_port: int) -> None:
    assert endpoint
    assert "://" not in endpoint, f"unexpected scheme in endpoint: {endpoint}"
    if "/" in endpoint:
        assert endpoint.endswith(f"/{expected_port}"), (
            f"endpoint route must end with /{expected_port}: {endpoint}"
        )
        assert endpoint.split("/", 1)[0], f"missing domain in endpoint: {endpoint}"
        return
    host, port = endpoint.rsplit(":", 1)
    assert host
    assert port.isdigit()
    assert int(port) == expected_port


def _assert_terminal_event_contract(
        *,
        init_events: list[ExecutionInit],
        completed_events: list[ExecutionComplete],
        errors: list[ExecutionError],
        execution_id: str | None,
) -> None:
    # Contract: init must exist, and at least one terminal signal exists.
    # For Jupyter-backed code execution, complete and error may coexist.
    assert len(init_events) == 1
    assert init_events[0].id is not None and init_events[0].id.strip()
    if execution_id is not None:
        assert init_events[0].id == execution_id
    _assert_recent_timestamp_ms(init_events[0].timestamp)
    assert (len(completed_events) > 0) or (len(errors) > 0), (
        f"expected at least one of complete/error, got complete={len(completed_events)} "
        f"error={len(errors)}"
    )
    if len(completed_events) > 0:
        assert len(completed_events) == 1
        _assert_recent_timestamp_ms(completed_events[0].timestamp)
        assert completed_events[0].execution_time_in_millis >= 0
    if len(errors) > 0:
        assert errors[0].name
        assert errors[0].value is not None
        _assert_recent_timestamp_ms(errors[0].timestamp)


def _buffer_attempt_handlers(
        handlers: ExecutionHandlers,
) -> tuple[ExecutionHandlers, Callable[[], Awaitable[None]]]:
    buffered_events: list[tuple[str, object]] = []

    async def on_stdout(msg: OutputMessage) -> None:
        buffered_events.append(("stdout", msg))

    async def on_stderr(msg: OutputMessage) -> None:
        buffered_events.append(("stderr", msg))

    async def on_result(result: ExecutionResult) -> None:
        buffered_events.append(("result", result))

    async def on_complete(complete: ExecutionComplete) -> None:
        buffered_events.append(("complete", complete))

    async def on_error(error: ExecutionError) -> None:
        buffered_events.append(("error", error))

    async def on_init(init: ExecutionInit) -> None:
        buffered_events.append(("init", init))

    async def flush() -> None:
        for event_type, payload in buffered_events:
            if event_type == "stdout" and handlers.on_stdout is not None:
                await handlers.on_stdout(payload)
            elif event_type == "stderr" and handlers.on_stderr is not None:
                await handlers.on_stderr(payload)
            elif event_type == "result" and handlers.on_result is not None:
                await handlers.on_result(payload)
            elif (
                event_type == "complete"
                and handlers.on_execution_complete is not None
            ):
                await handlers.on_execution_complete(payload)
            elif event_type == "error" and handlers.on_error is not None:
                await handlers.on_error(payload)
            elif event_type == "init" and handlers.on_init is not None:
                await handlers.on_init(payload)

    return (
        ExecutionHandlers(
            on_stdout=on_stdout if handlers.on_stdout is not None else None,
            on_stderr=on_stderr if handlers.on_stderr is not None else None,
            on_result=on_result if handlers.on_result is not None else None,
            on_execution_complete=(
                on_complete if handlers.on_execution_complete is not None else None
            ),
            on_error=on_error if handlers.on_error is not None else None,
            on_init=on_init if handlers.on_init is not None else None,
        ),
        flush,
    )


async def run_with_retry(
    code_interpreter: CodeInterpreter,
    code: str,
    *,
    context=None,
    language=None,
    handlers=None,
    max_retries: int = 3,
    retry_delay: float = 2.0,
    per_call_timeout: float = 120.0,
):
    """
    Run code with retry logic for flaky kernel initialization and network errors.

    Returns the execution result, retrying on:
    - Empty/None id responses (kernel not ready)
    - Network errors (connection reset, server disconnected)
    - Per-call timeout (SSE stream hangs due to peer disconnect)
    """
    last_result = None
    last_exception = None

    for attempt in range(max_retries):
        try:
            attempt_handlers = handlers
            flush_attempt_events: Callable[[], Awaitable[None]] | None = None
            if handlers is not None:
                attempt_handlers, flush_attempt_events = _buffer_attempt_handlers(
                    handlers
                )

            result = await asyncio.wait_for(
                code_interpreter.codes.run(
                    code,
                    context=context,
                    language=language,
                    handlers=attempt_handlers,
                ),
                timeout=per_call_timeout,
            )
            last_result = result
            if result is not None and result.id is not None:
                if flush_attempt_events is not None:
                    await flush_attempt_events()
                return result
            # Empty result - retry
            if attempt < max_retries - 1:
                logger.warning(
                    "Execution returned empty result (attempt %d/%d), retrying in %.1fs...",
                    attempt + 1, max_retries, retry_delay
                )
                await asyncio.sleep(retry_delay)
                retry_delay *= 1.5  # exponential backoff
        except asyncio.TimeoutError:
            last_exception = TimeoutError(
                f"codes.run() did not complete within {per_call_timeout}s"
            )
            if attempt < max_retries - 1:
                logger.warning(
                    "Execution timed out after %.0fs (attempt %d/%d), retrying in %.1fs...",
                    per_call_timeout, attempt + 1, max_retries, retry_delay,
                )
                await asyncio.sleep(retry_delay)
                retry_delay *= 1.5
            else:
                logger.error(
                    "Execution timed out after %.0fs on final attempt %d/%d",
                    per_call_timeout, attempt + 1, max_retries,
                )
        except Exception as e:
            last_exception = e
            error_name = type(e).__name__
            # Check if it's a retryable network error
            error_str = str(e).lower()
            is_retryable = any(keyword in error_str for keyword in [
                "disconnected", "connection", "reset", "closed", "timeout",
                "remoteerror", "protocol", "peer closed", "session is busy",
            ])
            if is_retryable and attempt < max_retries - 1:
                logger.warning(
                    "Execution failed with %s (attempt %d/%d), retrying in %.1fs: %s",
                    error_name, attempt + 1, max_retries, retry_delay, str(e)[:100]
                )
                await asyncio.sleep(retry_delay)
                retry_delay *= 1.5
            else:
                # Non-retryable error or last attempt
                raise

    # If we have a result (even empty), return it; otherwise raise last exception
    if last_result is not None:
        return last_result
    if last_exception is not None:
        raise last_exception
    return None


async def create_context_with_retry(
    code_interpreter: CodeInterpreter,
    language: str,
    max_retries: int = 3,
    retry_delay: float = 2.0,
):
    """Create a code context with retry logic for network errors."""
    last_exception = None
    for attempt in range(max_retries):
        try:
            ctx = await code_interpreter.codes.create_context(language)
            # Small delay to allow kernel initialization
            await asyncio.sleep(0.5)
            return ctx
        except Exception as e:
            last_exception = e
            error_str = str(e).lower()
            is_retryable = any(keyword in error_str for keyword in [
                "disconnected", "connection", "reset", "closed", "timeout",
                "remoteerror", "protocol", "peer closed"
            ])
            if is_retryable and attempt < max_retries - 1:
                logger.warning(
                    "Context creation failed (attempt %d/%d), retrying in %.1fs: %s",
                    attempt + 1, max_retries, retry_delay, str(e)[:100]
                )
                await asyncio.sleep(retry_delay)
                retry_delay *= 1.5
            else:
                raise
    raise last_exception  # type: ignore


@asynccontextmanager
async def managed_ctx(code_interpreter: CodeInterpreter, language: str):
    ctx = await create_context_with_retry(code_interpreter, language)
    try:
        yield ctx
    finally:
        # Best-effort cleanup with retry and a hard timeout so that an
        # unreachable sandbox (dead container / network gone) cannot block
        # the test suite indefinitely.
        for cleanup_attempt in range(2):
            try:
                if ctx.id:
                    await asyncio.wait_for(
                        code_interpreter.codes.delete_context(ctx.id),
                        timeout=10.0,
                    )
                break
            except Exception:
                if cleanup_attempt == 0:
                    await asyncio.sleep(0.5)
                else:
                    logger.warning(
                        "Cleanup: failed to delete context %s (%s)", ctx.id, language, exc_info=True
                    )


@asynccontextmanager
async def managed_ctx_stack(code_interpreter: CodeInterpreter, languages: list[str]):
    async with AsyncExitStack() as stack:
        contexts = []
        for lang in languages:
            contexts.append(await stack.enter_async_context(managed_ctx(code_interpreter, lang)))
        yield contexts


@pytest.mark.asyncio
class TestCodeInterpreterE2E:
    """Comprehensive E2E tests for CodeInterpreter runCode functionality (ordered)."""

    sandbox: Sandbox | None = None
    code_interpreter: CodeInterpreter | None = None
    connection_config: ConnectionConfig | None = None
    _setup_done = False

    @pytest.fixture(scope="class", autouse=True)
    async def _ci_lifecycle(self, request):
        """Create sandbox + code interpreter once and ALWAYS cleanup."""
        await request.cls._ensure_code_interpreter_created()
        try:
            yield
        finally:
            sandbox = request.cls.sandbox
            if sandbox is not None:
                try:
                    await sandbox.kill()
                except Exception as e:
                    logger.warning("Teardown: sandbox.kill() failed: %s", e, exc_info=True)
                try:
                    await sandbox.close()
                except Exception as e:
                    logger.warning("Teardown: sandbox.close() failed: %s", e, exc_info=True)

    @pytest.fixture
    async def isolated_code_interpreter(self):
        """Create an isolated sandbox+interpreter for high-flakiness run tests."""
        connection_config = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            entrypoint=["/opt/opensandbox/code-interpreter.sh"],
            connection_config=connection_config,
            resource=get_e2e_sandbox_resource(),
            timeout=timedelta(minutes=15),
            ready_timeout=timedelta(seconds=60),
            metadata={"tag": "e2e-code-interpreter-isolated"},
            env={
                "E2E_TEST": "true",
                "GO_VERSION": "1.25",
                "JAVA_VERSION": "21",
                "NODE_VERSION": "22",
                "PYTHON_VERSION": "3.12",
                "EXECD_LOG_FILE": "/tmp/opensandbox-e2e/logs/execd.log",
                "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms",
            },
            health_check_polling_interval=timedelta(milliseconds=500),
            volumes=[
                Volume(
                    name="execd-log",
                    host=Host(path="/tmp/opensandbox-e2e/logs"),
                    mountPath="/tmp/opensandbox-e2e/logs",
                    readOnly=False,
                ),
            ],
        )
        code_interpreter = await CodeInterpreter.create(sandbox=sandbox)
        try:
            yield code_interpreter
        finally:
            try:
                await sandbox.kill()
            except Exception as e:
                logger.warning("Teardown(isolated): sandbox.kill() failed: %s", e, exc_info=True)
            try:
                await sandbox.close()
            except Exception as e:
                logger.warning("Teardown(isolated): sandbox.close() failed: %s", e, exc_info=True)

    @classmethod
    async def _ensure_code_interpreter_created(cls) -> None:
        """Create CodeInterpreter once and reuse it across ordered tests."""
        if cls._setup_done:
            return

        logger.info("=" * 100)
        logger.info("SETUP: Creating sandbox and creating CodeInterpreter")
        logger.info("=" * 100)

        cls.connection_config = create_connection_config()

        cls.sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            entrypoint=["/opt/opensandbox/code-interpreter.sh"],
            connection_config=cls.connection_config,
            resource=get_e2e_sandbox_resource(),
            timeout=timedelta(minutes=15),
            ready_timeout=timedelta(seconds=60),
            metadata={"tag": "e2e-code-interpreter"},
            env={
                "E2E_TEST": "true",
                "GO_VERSION": "1.25",
                "JAVA_VERSION": "21",
                "NODE_VERSION": "22",
                "PYTHON_VERSION": "3.12",
                "EXECD_LOG_FILE": "/tmp/opensandbox-e2e/logs/execd.log",
                "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms",
            },
            health_check_polling_interval=timedelta(milliseconds=500),
            volumes=[
                Volume(
                    name="execd-log",
                    host=Host(path="/tmp/opensandbox-e2e/logs"),
                    mountPath="/tmp/opensandbox-e2e/logs",
                    readOnly=False,
                ),
            ],
        )

        cls.code_interpreter = await CodeInterpreter.create(sandbox=cls.sandbox)

        assert cls.code_interpreter is not None
        assert isinstance(cls.code_interpreter.id, str)
        logger.info("✓ CodeInterpreter created: %s", cls.code_interpreter.id)
        logger.info("=" * 100)

        cls._setup_done = True

    @pytest.mark.timeout(600)
    @pytest.mark.order(1)
    async def test_01_creation_and_basic_functionality(self):
        await self._ensure_code_interpreter_created()
        code_interpreter = TestCodeInterpreterE2E.code_interpreter
        assert code_interpreter is not None

        logger.info("=" * 80)
        logger.info("TEST 1: CodeInterpreter creation and basic functionality")
        logger.info("=" * 80)

        assert code_interpreter.codes is not None
        assert code_interpreter.files is not None
        assert code_interpreter.commands is not None
        assert code_interpreter.metrics is not None
        logger.info("✓ All service components are accessible")

        assert await code_interpreter.sandbox.is_healthy() is True
        logger.info("✓ CodeInterpreter is healthy")

        info = await code_interpreter.sandbox.get_info()
        assert str(code_interpreter.id) == str(info.id)
        # FIXME: upstream Kubernetes BatchSandbox lifecycle may still report
        # "Allocated" after execd health checks already pass. This E2E focuses
        # on end-to-end usability, so tolerate that transient state here.
        assert info.status.state in {"Running", "Allocated"}
        logger.info(
            "✓ CodeInterpreter info: state=%s, created=%s",
            info.status.state,
            info.created_at,
        )

        endpoint = await code_interpreter.sandbox.get_endpoint(DEFAULT_EXECD_PORT)
        assert endpoint is not None
        assert endpoint.endpoint is not None
        _assert_endpoint_has_port(endpoint.endpoint, DEFAULT_EXECD_PORT)
        logger.info("✓ CodeInterpreter endpoint: %s", endpoint.endpoint)

        metrics = await code_interpreter.sandbox.get_metrics()
        assert metrics is not None
        assert metrics.cpu_count > 0
        assert 0.0 <= metrics.cpu_used_percentage <= 100.0
        assert metrics.memory_total_in_mib > 0
        assert 0.0 <= metrics.memory_used_in_mib <= metrics.memory_total_in_mib
        _assert_recent_timestamp_ms(metrics.timestamp)
        logger.info(
            "✓ CPU: %s cores, %.2f%% used",
            metrics.cpu_count,
            metrics.cpu_used_percentage,
        )
        logger.info(
            "✓ Memory: %s/%s MiB",
            int(metrics.memory_used_in_mib),
            int(metrics.memory_total_in_mib),
        )

        # Renewal through CodeInterpreter (extend expiration time)
        renew_response = await code_interpreter.sandbox.renew(timedelta(minutes=20))
        assert renew_response is not None
        logger.info("✓ CodeInterpreter expiration renewed to %s", renew_response.expires_at)

        renewed_info = await code_interpreter.sandbox.get_info()
        assert abs((renewed_info.expires_at - renew_response.expires_at).total_seconds()) < 10
        now = renewed_info.expires_at.__class__.now(tz=renewed_info.expires_at.tzinfo)
        remaining = renewed_info.expires_at - now
        assert remaining > timedelta(minutes=18)
        assert remaining < timedelta(minutes=22)
        logger.info("✓ Expiration updated to %s", renewed_info.expires_at)

    @pytest.mark.timeout(900)
    @pytest.mark.order(2)
    async def test_02_java_code_execution(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 2: Java code execution")
        logger.info("=" * 80)

        async with managed_ctx(code_interpreter, SupportedLanguage.JAVA) as java_context:
            assert java_context.id is not None and java_context.id.strip()
            assert java_context.language == "java"
            logger.info("✓ Java context created")

            stdout_messages: list[OutputMessage] = []
            stderr_messages: list[OutputMessage] = []
            results: list[ExecutionResult] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            async def on_stdout(msg: OutputMessage):
                stdout_messages.append(msg)
                logger.info("Java stdout: %s", msg.text)

            async def on_stderr(msg: OutputMessage):
                stderr_messages.append(msg)
                logger.warning("Java stderr: %s", msg.text)

            async def on_result(result: ExecutionResult):
                results.append(result)
                logger.info("Java result: %s", result.text)

            async def on_complete(complete: ExecutionComplete):
                completed_events.append(complete)
                logger.info(
                    "Java execution completed in %s ms", complete.execution_time_in_millis
                )

            async def on_error(error: ExecutionError):
                errors.append(error)
                logger.error("Java error: %s - %s", error.name, error.value)

            async def on_init(init: ExecutionInit):
                init_events.append(init)
                logger.info("Java execution initialized with ID: %s", init.id)

            handlers = ExecutionHandlers(
                on_stdout=on_stdout,
                on_stderr=on_stderr,
                on_result=on_result,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            # Use retry for first execution in context (Java kernel init can be slow)
            simple_result = await run_with_retry(
                code_interpreter,
                "System.out.println(\"Hello from Java!\");\n"
                + "int result = 2 + 2;\n"
                + "System.out.println(\"2 + 2 = \" + result);\n"
                + "result",
                context=java_context,
                handlers=handlers,
                )
            assert simple_result is not None
            assert simple_result.id is not None and simple_result.id.strip()
            assert len(simple_result.result) > 0
            assert simple_result.result[0].text == "4"
            assert simple_result.exit_code is None
            assert simple_result.complete is not None

            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=simple_result.id,
            )
            assert len(errors) == 0
            assert len(completed_events) == 1
            assert len(stdout_messages) > 0
            assert any("Hello from Java!" in m.text for m in stdout_messages)
            # Depending on kernel formatting, spaces may vary; normalize spaces for matching.
            assert any(
                "2+2=4" in m.text.replace(" ", "") for m in stdout_messages
            )
            assert all(m.is_error is False for m in stdout_messages)
            for m in stdout_messages[:3]:
                _assert_recent_timestamp_ms(m.timestamp)
            logger.info("✓ Simple Java execution successful")

            var_result = await code_interpreter.codes.run(
                "import java.util.*;\n"
                + "List<Integer> numbers = Arrays.asList(1, 2, 3, 4, 5);\n"
                + "int sum = numbers.stream().mapToInt(Integer::intValue).sum();\n"
                + "System.out.println(\"Numbers: \" + numbers);\n"
                + "System.out.println(\"Sum: \" + sum);\n"
                + "result",
                context=java_context,
                )
            assert var_result is not None
            assert var_result.id is not None
            assert len(var_result.result) > 0
            assert var_result.result[0].text == "4"
            assert var_result.exit_code is None
            assert var_result.complete is not None
            logger.info("✓ Java variables and state persistence work correctly")

            # Error handling test
            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result = await code_interpreter.codes.run(
                "int x = 10 / 0; // This will cause ArithmeticException",
                context=java_context,
                handlers=handlers,
            )
            assert error_result is not None
            assert error_result.id is not None and error_result.id.strip()
            assert error_result.error is not None
            assert error_result.error.name == "EvalException"
            assert error_result.exit_code is None

            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=error_result.id,
            )
            assert len(errors) > 0
            assert errors[0].name == "EvalException"
            logger.info("✓ Java error handling works correctly")

    @pytest.mark.timeout(900)
    @pytest.mark.order(3)
    async def test_03_python_code_execution(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 3: Python code execution")
        logger.info("=" * 80)

        # New usage: directly pass a language string (ephemeral context).
        # This validates the `codes.run(..., language=...)` convenience interface.
        # Use retry helper for the first call — kernel initialization can be flaky.
        direct_lang_result = await run_with_retry(
            code_interpreter,
            "result = 2 + 2\nresult",
            language=SupportedLanguage.PYTHON,
        )
        assert direct_lang_result is not None
        assert direct_lang_result.id is not None and direct_lang_result.id.strip()
        assert direct_lang_result.error is None
        assert len(direct_lang_result.result) > 0
        assert direct_lang_result.result[0].text == "4"

        stdout_messages: list[OutputMessage] = []
        stderr_messages: list[OutputMessage] = []
        errors: list[ExecutionError] = []
        completed_events: list[ExecutionComplete] = []
        init_events: list[ExecutionInit] = []

        async def on_stdout(msg: OutputMessage):
            stdout_messages.append(msg)
            logger.info("Python stdout: %s", msg.text)

        async def on_stderr(msg: OutputMessage):
            stderr_messages.append(msg)
            logger.warning("Python stderr: %s", msg.text)

        async def on_complete(complete: ExecutionComplete):
            completed_events.append(complete)
            logger.info(
                "Python execution completed in %s ms", complete.execution_time_in_millis
            )

        async def on_error(error: ExecutionError):
            errors.append(error)
            logger.error("Python error: %s - %s", error.name, error.value)

        async def on_init(init: ExecutionInit):
            init_events.append(init)
            logger.info("Python execution initialized with ID: %s", init.id)

        handlers_py = ExecutionHandlers(
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_execution_complete=on_complete,
            on_error=on_error,
            on_init=on_init,
        )

        async with managed_ctx(code_interpreter, SupportedLanguage.PYTHON) as python_context:
            assert python_context.id is not None and python_context.id.strip()
            logger.info("✓ Python context created")

            # Use retry for first execution in context (kernel init can be flaky)
            simple_result_py = await run_with_retry(
                code_interpreter,
                "print('Hello from Python!')\n"
                + "result = 2 + 2\n"
                + "print(f'2 + 2 = {result}')",
                context=python_context,
                handlers=handlers_py,
                )
            assert simple_result_py is not None
            assert simple_result_py.id is not None and simple_result_py.id.strip()

            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=simple_result_py.id,
            )
            assert len(errors) == 0
            assert len(completed_events) == 1
            assert any("Hello from Python!" in m.text for m in stdout_messages)
            assert any("2 + 2 = 4" in m.text for m in stdout_messages)
            logger.info("✓ Simple Python execution successful")

            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            var_result_py = await code_interpreter.codes.run(
                "x = 42\n"
                + "y = 'persistent variable'\n"
                + "my_list = [1, 2, 3, 4, 5]\n"
                + "print(f'x={x}, y=\"{y}\", list={my_list}')\n"
                + "result",
                context=python_context,
                handlers=handlers_py,
                )
            assert var_result_py is not None
            assert var_result_py.id is not None and var_result_py.id.strip()
            assert len(var_result_py.result) > 0
            assert var_result_py.result[0].text == "4"
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=var_result_py.id,
            )
            logger.info("✓ Python variables and state persistence work correctly")

            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()
            persist_result = await code_interpreter.codes.run(
                "print(f'Previously set variables: x={x}, y={y}')\n"
                + "z = sum(my_list)\n"
                + "print(f'Sum of list: {z}')",
                context=python_context,
                handlers=handlers_py,
                )
            assert persist_result is not None
            assert persist_result.id is not None and persist_result.id.strip()
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=persist_result.id,
            )
            assert any("Previously set variables: x=42" in m.text for m in stdout_messages)
            assert any("Sum of list: 15" in m.text for m in stdout_messages)
            logger.info("✓ Python variable persistence across executions works")

            # Error handling
            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result_py = await code_interpreter.codes.run(
                "print(undefined_variable)  # This will cause NameError",
                context=python_context,
                handlers=handlers_py,
            )
            assert error_result_py is not None
            assert error_result_py.id is not None and error_result_py.id.strip()
            assert error_result_py.error is not None or len(error_result_py.logs.stderr) > 0

            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=error_result_py.id,
            )
            assert len(errors) > 0
            if error_result_py.error:
                assert (
                        "NameError" in error_result_py.error.name
                        or "NameError" in error_result_py.error.value
                )
            assert "NameError" in errors[0].name or "NameError" in errors[0].value
            logger.info("✓ Python error handling works correctly")

    @pytest.mark.timeout(900)
    @pytest.mark.order(4)
    async def test_04_go_code_execution(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 4: Go code execution")
        logger.info("=" * 80)

        async with managed_ctx(code_interpreter, SupportedLanguage.GO) as go_context:
            assert go_context.id is not None and go_context.id.strip()
            assert go_context.language == "go"
            logger.info("✓ Go context created")

            stdout_messages: list[OutputMessage] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            async def on_stdout(msg: OutputMessage):
                stdout_messages.append(msg)
                logger.info("Go stdout: %s", msg.text)

            async def on_complete(complete: ExecutionComplete):
                completed_events.append(complete)
                logger.info("Go execution completed in %s ms", complete.execution_time_in_millis)

            async def on_error(error: ExecutionError):
                errors.append(error)
                logger.error("Go error: %s - %s", error.name, error.value)

            async def on_init(init: ExecutionInit):
                init_events.append(init)
                logger.info("Go execution initialized with ID: %s", init.id)

            handlers_go = ExecutionHandlers(
                on_stdout=on_stdout,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            # Use retry for first execution in context (Go compile can be slow)
            simple_result_go = await run_with_retry(
                code_interpreter,
                "package main\n"
                + "import \"fmt\"\n"
                + "func main() {\n"
                + "    fmt.Print(\"Hello from Go!\")\n"
                + "    result := 2 + 2\n"
                + "    fmt.Print(\"2 + 2 =\", result)\n"
                + "}",
                context=go_context,
                handlers=handlers_go,
                )
            assert simple_result_go is not None
            assert simple_result_go.id is not None and simple_result_go.id.strip()
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=simple_result_go.id,
            )
            assert len(errors) == 0
            assert len(completed_events) == 1
            assert len(stdout_messages) > 0
            logger.info("✓ Simple Go execution successful")

            data_result_go = await code_interpreter.codes.run(
                "package main\n"
                + "import \"fmt\"\n"
                + "func calculate(numbers []int) int {\n"
                + "    sum := 0\n"
                + "    for _, num := range numbers {\n"
                + "        sum += num\n"
                + "    }\n"
                + "    return sum\n"
                + "}\n"
                + "func main() {\n"
                + "    numbers := []int{1, 2, 3, 4, 5}\n"
                + "    sum := calculate(numbers)\n"
                + "    fmt.Print(\"Numbers:\", numbers)\n"
                + "    fmt.Print(\"Sum:\", sum)\n"
                + "}",
                context=go_context,
                )
            assert data_result_go is not None
            assert data_result_go.id is not None
            logger.info("✓ Go data structures and functions work correctly")

            # Compilation error
            stdout_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result_go = await code_interpreter.codes.run(
                "package main\n"
                + "func main() {\n"
                + "    undeclaredVariable++  // This will cause compilation error\n"
                + "}",
                context=go_context,
                handlers=handlers_go,
                )
            assert error_result_go is not None
            assert error_result_go.id is not None and error_result_go.id.strip()
            assert error_result_go.error is not None or len(error_result_go.logs.stderr) > 0
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=error_result_go.id,
            )
            logger.info("✓ Go error handling works correctly")

    @pytest.mark.timeout(900)
    @pytest.mark.order(5)
    async def test_05_typescript_code_execution(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 5: TypeScript code execution")
        logger.info("=" * 80)

        async with managed_ctx(code_interpreter, SupportedLanguage.TYPESCRIPT) as ts_context:
            assert ts_context.id is not None and ts_context.id.strip()
            assert ts_context.language == "typescript"
            logger.info("✓ TypeScript context created")

            stdout_messages: list[OutputMessage] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            async def on_stdout(msg: OutputMessage):
                stdout_messages.append(msg)
                logger.info("TypeScript stdout: %s", msg.text)

            async def on_complete(complete: ExecutionComplete):
                completed_events.append(complete)
                logger.info(
                    "TypeScript execution completed in %s ms", complete.execution_time_in_millis
                )

            async def on_error(error: ExecutionError):
                errors.append(error)
                logger.error("TypeScript error: %s - %s", error.name, error.value)

            async def on_init(init: ExecutionInit):
                init_events.append(init)
                logger.info("TypeScript execution initialized with ID: %s", init.id)

            handlers_ts = ExecutionHandlers(
                on_stdout=on_stdout,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            # Use retry for first execution in context (TS init can be slow)
            simple_result_ts = await run_with_retry(
                code_interpreter,
                "console.log('Hello from TypeScript!');\n"
                + "const result: number = 2 + 2;\n"
                + "console.log(`2 + 2 = ${result}`);",
                context=ts_context,
                handlers=handlers_ts,
                )
            assert simple_result_ts is not None
            assert simple_result_ts.id is not None and simple_result_ts.id.strip()
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=simple_result_ts.id,
            )
            assert len(errors) == 0
            assert len(completed_events) == 1
            assert any("Hello from TypeScript!" in m.text for m in stdout_messages)
            logger.info("✓ Simple TypeScript execution successful")

            types_result_ts = await code_interpreter.codes.run(
                "interface Person {\n"
                + "  name: string;\n"
                + "  age: number;\n"
                + "}\n"
                + "const person: Person = { name: 'John', age: 30 };\n"
                + "const numbers: number[] = [1, 2, 3, 4, 5];\n"
                + "const sum: number = numbers.reduce((a, b) => a + b, 0);\n"
                + "console.log(`Person: ${person.name}, Age: ${person.age}`);\n"
                + "console.log(`Numbers: ${numbers}`);\n"
                + "console.log(`Sum: ${sum}`);",
                context=ts_context,
                )
            assert types_result_ts is not None
            assert types_result_ts.id is not None
            logger.info("✓ TypeScript types and interfaces work correctly")

            # Type error
            stdout_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            # Use a deterministic runtime error (TypeScript compile/type-checking may be configured permissively).
            error_result_ts = await code_interpreter.codes.run(
                "throw new Error('ts-runtime-error');",
                context=ts_context,
                handlers=handlers_ts,
            )
            assert error_result_ts is not None
            assert error_result_ts.id is not None and error_result_ts.id.strip()
            assert error_result_ts.error is not None or len(error_result_ts.logs.stderr) > 0
            _assert_terminal_event_contract(
                init_events=init_events,
                completed_events=completed_events,
                errors=errors,
                execution_id=error_result_ts.id,
            )
            logger.info("✓ TypeScript error handling works correctly")

    @pytest.mark.timeout(900)
    @pytest.mark.order(6)
    async def test_06_multi_language_support_and_context_isolation(
            self, isolated_code_interpreter: CodeInterpreter
    ):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 6: Multi-language support and context isolation")
        logger.info("=" * 80)

        async with managed_ctx_stack(
            code_interpreter,
            [
                SupportedLanguage.PYTHON,
                SupportedLanguage.PYTHON,
                SupportedLanguage.JAVA,
                SupportedLanguage.GO,
            ],
        ) as (python1, python2, java1, go1):
            logger.info("✓ Created multiple contexts for different languages")

            # Use retry helper for flaky kernel initialization
            result1 = await run_with_retry(
                code_interpreter,
                "secret_value1 = 'python1_secret'\nprint(f'Python1 secret: {secret_value1}')",
                context=python1,
            )
            result2 = await run_with_retry(
                code_interpreter,
                "secret_value2 = 'python2_secret'\nprint(f'Python2 secret: {secret_value2}')",
                context=python2,
            )
            assert result1 is not None and result1.id is not None
            assert result2 is not None and result2.id is not None
            logger.info("✓ Variables set in different Python contexts")

            check1 = await code_interpreter.codes.run(
                "print(f'Python1 still has: {secret_value1}')",
                context=python1,
            )
            check2 = await code_interpreter.codes.run(
                "print(f'Python2 has no: {secret_value1}')",
                context=python2,
            )
            assert check1 is not None
            assert check2 is not None
            assert check2.error is not None
            assert check2.error.name == "NameError"
            logger.info("✓ Context isolation verified - contexts are properly isolated")

            java_result = await run_with_retry(
                code_interpreter,
                "String javaSecret = \"java_secret\";\n"
                + "System.out.println(\"Java secret: \" + javaSecret);",
                context=java1,
                )
            go_result = await run_with_retry(
                code_interpreter,
                "package main\n"
                + "import \"fmt\"\n"
                + "func main() {\n"
                + "    goSecret := \"go_secret\"\n"
                + "    fmt.Print(\"Go secret:\", goSecret)\n"
                + "}",
                context=go1,
                )
            assert java_result is not None and java_result.id is not None
            assert go_result is not None and go_result.id is not None
            logger.info("✓ Cross-language execution works correctly")

    @pytest.mark.timeout(900)
    @pytest.mark.order(7)
    async def test_07_concurrent_code_execution(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 7: Concurrent code execution")
        logger.info("=" * 80)

        async with managed_ctx_stack(
            code_interpreter,
            [
                SupportedLanguage.PYTHON,
                SupportedLanguage.JAVA,
                SupportedLanguage.GO,
            ],
        ) as (python_c1, java_c1, go_c1):
            logger.info("✓ Created contexts for concurrent execution")

            async def run_python1():
                return await code_interpreter.codes.run(
                    "import time\n"
                    + "for i in range(3):\n"
                    + "    print(f'Python1 iteration {i}')\n"
                    + "    time.sleep(0.1)\n"
                    + "print('Python1 completed')",
                    context=python_c1,
                    )

            async def run_java_concurrent():
                return await code_interpreter.codes.run(
                    "for (int i = 0; i < 3; i++) {\n"
                    + "    System.out.println(\"Java iteration \" + i);\n"
                    + "    try { Thread.sleep(100); } catch (Exception e) {}\n"
                    + "}\n"
                    + "System.out.println(\"Java completed\");",
                    context=java_c1,
                    )

            async def run_go_concurrent():
                return await code_interpreter.codes.run(
                    "package main\n"
                    + "import \"fmt\"\n"
                    + "func main() {\n"
                    + "    for i := 0; i < 3; i++ {\n"
                    + "        fmt.Print(\"Go iteration\", i)\n"
                    + "    }\n"
                    + "    fmt.Print(\"Go completed\")\n"
                    + "}",
                    context=go_c1,
                    )

            results = await asyncio.gather(
                run_python1(), run_java_concurrent(), run_go_concurrent()
            )
            for result in results:
                assert result is not None
                assert result.id is not None
                logger.info("✓ Concurrent execution completed: %s", result.id)

    @pytest.mark.timeout(900)
    @pytest.mark.order(8)
    async def test_08_code_execution_interrupt(self, isolated_code_interpreter: CodeInterpreter):
        code_interpreter = isolated_code_interpreter

        logger.info("=" * 80)
        logger.info("TEST 8: Code execution interrupt")
        logger.info("=" * 80)

        async with managed_ctx(code_interpreter, SupportedLanguage.PYTHON) as python_int_context:
            assert python_int_context.id is not None and python_int_context.id.strip()

            init_events_int: list[ExecutionInit] = []
            completed_events: list[ExecutionComplete] = []
            errors: list[ExecutionError] = []
            init_received = asyncio.Event()

            async def on_init(init: ExecutionInit):
                init_events_int.append(init)
                init_received.set()

            async def on_complete(complete: ExecutionComplete):
                completed_events.append(complete)

            async def on_error(error: ExecutionError):
                errors.append(error)

            handlers_int = ExecutionHandlers(
                on_init=on_init,
                on_execution_complete=on_complete,
                on_error=on_error,
            )

            execution_task = asyncio.create_task(
                code_interpreter.codes.run(
                    "import time\n"
                    + "print('Starting long-running Python execution')\n"
                    + "for i in range(100):\n"
                    + "    print(f'Python iteration {i}')\n"
                    + "    time.sleep(0.2)\n",
                    context=python_int_context,
                    handlers=handlers_int,
                    )
            )

            await asyncio.wait_for(init_received.wait(), timeout=15)
            assert len(init_events_int) == 1, "Execution should have been initialized exactly once"
            execution_id = init_events_int[-1].id
            assert execution_id is not None
            logger.info("✓ Execution initialized with ID: %s", execution_id)

            await asyncio.wait_for(
                code_interpreter.codes.interrupt(execution_id),
                timeout=15.0,
            )

            # After interrupt the SSE stream should close promptly.  Add a
            # hard timeout so that a slow/stuck server cannot block the test
            # for the full 900 s pytest-timeout.
            try:
                result_int = await asyncio.wait_for(execution_task, timeout=60.0)
            except (asyncio.TimeoutError, Exception) as exc:
                execution_task.cancel()
                logger.warning(
                    "Execution task did not return cleanly after interrupt: %s", exc
                )
                result_int = None

            if result_int is not None:
                assert result_int.id is not None
                assert result_int.id == execution_id

            quick_result = None
            try:
                quick_result = await asyncio.wait_for(
                    code_interpreter.codes.run(
                        "print('Quick Python execution')\n"
                        + "result = 2 + 2\n"
                        + "print(f'Result: {result}')",
                        context=python_int_context,
                        handlers=handlers_int,
                    ),
                    timeout=60.0,
                )
                assert quick_result is not None
                assert quick_result.id is not None
            except (asyncio.TimeoutError, Exception) as exc:
                logger.warning("Quick execution after interrupt failed: %s", exc)

            # Different backends may close the interrupted SSE stream without
            # emitting an explicit terminal event. Accept either a terminal
            # event or proof that the context became usable again.
            assert (
                len(completed_events) > 0
                or len(errors) > 0
                or quick_result is not None
            ), "expected terminal event or successful follow-up execution after interrupt"
            logger.info("✓ Python execution was interrupted successfully")

            # Interrupting a completed execution may or may not throw depending on backend behavior.
            try:
                if quick_result is not None:
                    await asyncio.wait_for(
                        code_interpreter.codes.interrupt(quick_result.id),
                        timeout=10.0,
                    )
            except Exception:
                pass

    @pytest.mark.timeout(600)
    @pytest.mark.order(9)
    async def test_09_context_management_endpoints(self):
        """Validate list/get/delete context APIs map to execd /code/contexts endpoints."""
        await self._ensure_code_interpreter_created()
        code_interpreter = TestCodeInterpreterE2E.code_interpreter
        assert code_interpreter is not None

        language = SupportedLanguage.BASH
        logger.info("=" * 80)
        logger.info("TEST 9: Context management endpoints (%s)", language)
        logger.info("=" * 80)

        # Ensure clean slate for bash contexts to avoid interference with other tests.
        await code_interpreter.codes.delete_contexts(language)

        ctx1 = await code_interpreter.codes.create_context(language)
        ctx2 = await code_interpreter.codes.create_context(language)
        assert ctx1.id is not None and ctx1.id.strip()
        assert ctx2.id is not None and ctx2.id.strip()
        assert ctx1.language == language
        assert ctx2.language == language
        logger.info("✓ Created two bash contexts: %s, %s", ctx1.id, ctx2.id)

        listed = await code_interpreter.codes.list_contexts(language)
        bash_context_ids = {c.id for c in listed if c.id}
        assert ctx1.id in bash_context_ids
        assert ctx2.id in bash_context_ids
        assert all(c.language == language for c in listed)
        logger.info("✓ list_contexts returned expected bash contexts")

        fetched = await code_interpreter.codes.get_context(ctx1.id)
        assert fetched.id == ctx1.id
        assert fetched.language == language
        logger.info("✓ get_context returned expected context %s", fetched.id)

        await code_interpreter.codes.delete_context(ctx1.id)
        remaining = await code_interpreter.codes.list_contexts(language)
        remaining_ids = {c.id for c in remaining if c.id}
        assert ctx1.id not in remaining_ids
        assert ctx2.id in remaining_ids
        logger.info("✓ delete_context removed %s", ctx1.id)

        await code_interpreter.codes.delete_contexts(language)
        final_contexts = [
            c for c in await code_interpreter.codes.list_contexts(language) if c.id
        ]
        assert len(final_contexts) == 0
        logger.info("✓ delete_contexts removed all bash contexts")
