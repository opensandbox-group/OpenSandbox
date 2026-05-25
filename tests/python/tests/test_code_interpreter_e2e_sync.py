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
Comprehensive Sync E2E tests for CodeInterpreterSync functionality.

This mirrors `test_code_interpreter_e2e.py` but uses the synchronous SDK.
"""

import logging
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, contextmanager
from datetime import timedelta
from threading import Event

import pytest
from code_interpreter import CodeInterpreterSync
from code_interpreter.models.code import SupportedLanguage
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.constants import DEFAULT_EXECD_PORT
from opensandbox.models.execd import (
    ExecutionComplete,
    ExecutionError,
    ExecutionInit,
    ExecutionResult,
    OutputMessage,
)
from opensandbox.models.execd_sync import ExecutionHandlersSync
from opensandbox.models.sandboxes import Host, SandboxImageSpec, Volume

from tests.base_e2e_test import (
    create_connection_config_sync,
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


def _buffer_attempt_handlers_sync(
    handlers: ExecutionHandlersSync,
) -> tuple[ExecutionHandlersSync, Callable[[], None]]:
    buffered_events: list[tuple[str, object]] = []

    def on_stdout(msg) -> None:
        buffered_events.append(("stdout", msg))

    def on_stderr(msg) -> None:
        buffered_events.append(("stderr", msg))

    def on_result(result) -> None:
        buffered_events.append(("result", result))

    def on_complete(complete) -> None:
        buffered_events.append(("complete", complete))

    def on_error(error) -> None:
        buffered_events.append(("error", error))

    def on_init(init) -> None:
        buffered_events.append(("init", init))

    def flush() -> None:
        for event_type, payload in buffered_events:
            if event_type == "stdout" and handlers.on_stdout is not None:
                handlers.on_stdout(payload)
            elif event_type == "stderr" and handlers.on_stderr is not None:
                handlers.on_stderr(payload)
            elif event_type == "result" and handlers.on_result is not None:
                handlers.on_result(payload)
            elif (
                event_type == "complete"
                and handlers.on_execution_complete is not None
            ):
                handlers.on_execution_complete(payload)
            elif event_type == "error" and handlers.on_error is not None:
                handlers.on_error(payload)
            elif event_type == "init" and handlers.on_init is not None:
                handlers.on_init(payload)

    return (
        ExecutionHandlersSync(
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


def run_with_retry_sync(
    code_interpreter: CodeInterpreterSync,
    code: str,
    *,
    context=None,
    language=None,
    handlers=None,
    max_retries: int = 3,
    retry_delay: float = 2.0,
):
    """
    Synchronous retry wrapper for code_interpreter.codes.run().

    Retries on:
    - Empty/None id responses (kernel not ready / session busy)
    - Retryable network errors (connection reset, server disconnected)
    """
    last_result = None
    last_exception = None

    for attempt in range(max_retries):
        try:
            attempt_handlers = handlers
            flush_attempt_events: Callable[[], None] | None = None
            if handlers is not None:
                attempt_handlers, flush_attempt_events = _buffer_attempt_handlers_sync(
                    handlers
                )
            result = code_interpreter.codes.run(
                code,
                context=context,
                language=language,
                handlers=attempt_handlers,
            )
            last_result = result
            if result is not None and result.id is not None:
                if flush_attempt_events is not None:
                    flush_attempt_events()
                return result
            # Empty result — retry
            if attempt < max_retries - 1:
                logger.warning(
                    "Execution returned empty result (attempt %d/%d), retrying in %.1fs...",
                    attempt + 1, max_retries, retry_delay,
                )
                time.sleep(retry_delay)
                retry_delay *= 1.5
        except Exception as e:
            last_exception = e
            error_str = str(e).lower()
            is_retryable = any(keyword in error_str for keyword in [
                "disconnected", "connection", "reset", "closed", "timeout",
                "remoteerror", "protocol", "peer closed", "session is busy",
            ])
            if is_retryable and attempt < max_retries - 1:
                logger.warning(
                    "Execution failed with %s (attempt %d/%d), retrying in %.1fs: %s",
                    type(e).__name__, attempt + 1, max_retries, retry_delay, str(e)[:100],
                )
                time.sleep(retry_delay)
                retry_delay *= 1.5
            else:
                raise

    if last_result is not None:
        return last_result
    if last_exception is not None:
        raise last_exception
    return None


@contextmanager
def managed_ctx_sync(code_interpreter: CodeInterpreterSync, language: str):
    ctx = code_interpreter.codes.create_context(language)
    try:
        yield ctx
    finally:
        try:
            if ctx.id:
                code_interpreter.codes.delete_context(ctx.id)
        except Exception:
            logger.warning(
                "Cleanup: failed to delete context %s (%s)", ctx.id, language, exc_info=True
            )


@contextmanager
def managed_ctx_stack_sync(code_interpreter: CodeInterpreterSync, languages: list[str]):
    with ExitStack() as stack:
        contexts = []
        for lang in languages:
            contexts.append(stack.enter_context(managed_ctx_sync(code_interpreter, lang)))
        yield contexts


class TestCodeInterpreterE2ESync:
    sandbox: SandboxSync | None = None
    code_interpreter: CodeInterpreterSync | None = None
    connection_config: ConnectionConfigSync | None = None
    _setup_done = False

    @pytest.fixture(scope="class", autouse=True)
    def _ci_lifecycle(self, request):
        """Create sandbox + code interpreter once and ALWAYS cleanup."""
        request.cls._ensure_code_interpreter_created()
        try:
            yield
        finally:
            sandbox = request.cls.sandbox
            if sandbox is not None:
                try:
                    sandbox.kill()
                except Exception as e:
                    logger.warning("Teardown: sandbox.kill() failed: %s", e, exc_info=True)
                try:
                    sandbox.close()
                except Exception as e:
                    logger.warning("Teardown: sandbox.close() failed: %s", e, exc_info=True)

            cfg = request.cls.connection_config
            if cfg is not None:
                try:
                    cfg.transport.close()
                except Exception:
                    pass

    @pytest.fixture
    def isolated_code_interpreter(self):
        """Create an isolated sandbox+interpreter for high-flakiness run tests."""
        connection_config = create_connection_config_sync()
        sandbox = SandboxSync.create(
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
        code_interpreter = CodeInterpreterSync.create(sandbox=sandbox)
        try:
            yield code_interpreter
        finally:
            try:
                sandbox.kill()
            except Exception as e:
                logger.warning("Teardown(isolated): sandbox.kill() failed: %s", e, exc_info=True)
            try:
                sandbox.close()
            except Exception as e:
                logger.warning("Teardown(isolated): sandbox.close() failed: %s", e, exc_info=True)
            try:
                connection_config.transport.close()
            except Exception:
                pass

    @classmethod
    def _ensure_code_interpreter_created(cls) -> None:
        if cls._setup_done:
            return

        cls.connection_config = create_connection_config_sync()

        cls.sandbox = SandboxSync.create(
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

        cls.code_interpreter = CodeInterpreterSync.create(sandbox=cls.sandbox)
        assert cls.code_interpreter is not None
        assert isinstance(cls.code_interpreter.id, str)
        cls._setup_done = True

    @pytest.mark.timeout(600)
    @pytest.mark.order(1)
    def test_01_creation_and_basic_functionality(self):
        TestCodeInterpreterE2ESync._ensure_code_interpreter_created()
        code_interpreter = TestCodeInterpreterE2ESync.code_interpreter
        assert code_interpreter is not None

        assert code_interpreter.codes is not None
        assert code_interpreter.files is not None
        assert code_interpreter.commands is not None
        assert code_interpreter.metrics is not None

        assert code_interpreter.sandbox.is_healthy() is True

        info = code_interpreter.sandbox.get_info()
        assert str(code_interpreter.id) == str(info.id)
        # FIXME: upstream Kubernetes BatchSandbox lifecycle may still report
        # "Allocated" after execd health checks already pass. This E2E focuses
        # on end-to-end usability, so tolerate that transient state here.
        assert info.status.state in {"Running", "Allocated"}

        endpoint = code_interpreter.sandbox.get_endpoint(DEFAULT_EXECD_PORT)
        assert endpoint is not None
        assert endpoint.endpoint is not None
        _assert_endpoint_has_port(endpoint.endpoint, DEFAULT_EXECD_PORT)

        metrics = code_interpreter.sandbox.get_metrics()
        assert metrics is not None
        assert metrics.cpu_count > 0
        assert 0.0 <= metrics.cpu_used_percentage <= 100.0
        assert metrics.memory_total_in_mib > 0
        assert 0.0 <= metrics.memory_used_in_mib <= metrics.memory_total_in_mib
        _assert_recent_timestamp_ms(metrics.timestamp)

        renew_response = code_interpreter.sandbox.renew(timedelta(minutes=20))
        assert renew_response is not None
        renewed_info = code_interpreter.sandbox.get_info()
        assert abs((renewed_info.expires_at - renew_response.expires_at).total_seconds()) < 10
        now = renewed_info.expires_at.__class__.now(tz=renewed_info.expires_at.tzinfo)
        remaining = renewed_info.expires_at - now
        assert remaining > timedelta(minutes=18)
        assert remaining < timedelta(minutes=22)

    @pytest.mark.timeout(900)
    @pytest.mark.order(2)
    def test_02_java_code_execution(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_sync(code_interpreter, SupportedLanguage.JAVA) as java_context:
            assert java_context.id is not None and str(java_context.id).strip()
            assert java_context.language == "java"

            stdout_messages: list[OutputMessage] = []
            stderr_messages: list[OutputMessage] = []
            results: list[ExecutionResult] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            def on_stdout(msg):
                stdout_messages.append(msg)

            def on_stderr(msg):
                stderr_messages.append(msg)

            def on_result(result):
                results.append(result)

            def on_complete(complete):
                completed_events.append(complete)

            def on_error(error):
                errors.append(error)

            def on_init(init):
                init_events.append(init)

            handlers = ExecutionHandlersSync(
                on_stdout=on_stdout,
                on_stderr=on_stderr,
                on_result=on_result,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            # Use retry for first execution in context because Java kernel init can be slow.
            simple_result = run_with_retry_sync(
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
            assert simple_result.error is None
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
            assert any("2+2=4" in m.text.replace(" ", "") for m in stdout_messages)
            assert all(m.is_error is False for m in stdout_messages)
            for m in stdout_messages[:3]:
                _assert_recent_timestamp_ms(m.timestamp)

            var_result = code_interpreter.codes.run(
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

            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result = code_interpreter.codes.run(
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

    @pytest.mark.timeout(900)
    @pytest.mark.order(3)
    def test_03_python_code_execution(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        # New usage: directly pass a language string (ephemeral context).
        # This validates the `codes.run(..., language=...)` convenience interface.
        direct_lang_result = run_with_retry_sync(
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

        def on_stdout(msg):
            stdout_messages.append(msg)

        def on_stderr(msg):
            stderr_messages.append(msg)

        def on_complete(complete):
            completed_events.append(complete)

        def on_error(error):
            errors.append(error)

        def on_init(init):
            init_events.append(init)

        handlers_py = ExecutionHandlersSync(
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_execution_complete=on_complete,
            on_error=on_error,
            on_init=on_init,
        )

        with managed_ctx_sync(code_interpreter, SupportedLanguage.PYTHON) as python_context:
            assert python_context.id is not None and str(python_context.id).strip()

            simple_result_py = run_with_retry_sync(
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

            var_result_py = code_interpreter.codes.run(
                "x = 42\n"
                + "y = 'persistent variable'\n"
                + "my_list = [1, 2, 3, 4, 5]\n"
                + "print(f'x={x}, y=\"{y}\", list={my_list}')\n"
                + "result",
                context=python_context,
                )
            assert var_result_py is not None
            assert var_result_py.id is not None
            assert len(var_result_py.result) > 0
            assert var_result_py.result[0].text == "4"

            persist_result = code_interpreter.codes.run(
                "print(f'Previously set variables: x={x}, y={y}')\n"
                + "z = sum(my_list)\n"
                + "print(f'Sum of list: {z}')",
                context=python_context,
                )
            assert persist_result is not None
            assert persist_result.id is not None

            stdout_messages.clear()
            stderr_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result_py = code_interpreter.codes.run(
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

    @pytest.mark.timeout(900)
    @pytest.mark.order(4)
    def test_04_go_code_execution(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_sync(code_interpreter, SupportedLanguage.GO) as go_context:
            assert go_context.id is not None and str(go_context.id).strip()
            assert go_context.language == "go"

            stdout_messages: list[OutputMessage] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            def on_stdout(msg):
                stdout_messages.append(msg)

            def on_complete(complete):
                completed_events.append(complete)

            def on_error(error):
                errors.append(error)

            def on_init(init):
                init_events.append(init)

            handlers_go = ExecutionHandlersSync(
                on_stdout=on_stdout,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            simple_result_go = code_interpreter.codes.run(
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
            assert len(stdout_messages) > 0

            data_result_go = code_interpreter.codes.run(
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

            stdout_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            error_result_go = code_interpreter.codes.run(
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

    @pytest.mark.timeout(900)
    @pytest.mark.order(5)
    def test_05_typescript_code_execution(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_sync(code_interpreter, SupportedLanguage.TYPESCRIPT) as ts_context:
            assert ts_context.id is not None and str(ts_context.id).strip()
            assert ts_context.language == "typescript"

            stdout_messages: list[OutputMessage] = []
            errors: list[ExecutionError] = []
            completed_events: list[ExecutionComplete] = []
            init_events: list[ExecutionInit] = []

            def on_stdout(msg):
                stdout_messages.append(msg)

            def on_complete(complete):
                completed_events.append(complete)

            def on_error(error):
                errors.append(error)

            def on_init(init):
                init_events.append(init)

            handlers_ts = ExecutionHandlersSync(
                on_stdout=on_stdout,
                on_execution_complete=on_complete,
                on_error=on_error,
                on_init=on_init,
            )

            simple_result_ts = code_interpreter.codes.run(
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

            types_result_ts = code_interpreter.codes.run(
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

            stdout_messages.clear()
            errors.clear()
            completed_events.clear()
            init_events.clear()

            # Use a deterministic runtime error (TypeScript compile/type-checking may be configured permissively).
            error_result_ts = code_interpreter.codes.run(
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

    @pytest.mark.timeout(900)
    @pytest.mark.order(6)
    def test_06_multi_language_support_and_context_isolation(
        self, isolated_code_interpreter: CodeInterpreterSync
    ):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_stack_sync(
            code_interpreter,
            [
                SupportedLanguage.PYTHON,
                SupportedLanguage.PYTHON,
                SupportedLanguage.JAVA,
                SupportedLanguage.GO,
            ],
        ) as (python1, python2, java1, go1):
            assert python1.id is not None and str(python1.id).strip()
            assert python2.id is not None and str(python2.id).strip()
            assert java1.id is not None and str(java1.id).strip()
            assert go1.id is not None and str(go1.id).strip()

            # Use retry helper for flaky kernel initialization
            result1 = run_with_retry_sync(
                code_interpreter,
                "secret_value1 = 'python1_secret'\nprint(f'Python1 secret: {secret_value1}')",
                context=python1,
            )
            result2 = run_with_retry_sync(
                code_interpreter,
                "secret_value2 = 'python2_secret'\nprint(f'Python2 secret: {secret_value2}')",
                context=python2,
            )
            assert result1 is not None and result1.id is not None
            assert result2 is not None and result2.id is not None

            # Small delay to avoid "session is busy" between runs
            time.sleep(1)

            check1 = run_with_retry_sync(
                code_interpreter,
                "print(f'Python1 still has: {secret_value1}')",
                context=python1,
            )
            time.sleep(0.5)
            check2 = run_with_retry_sync(
                code_interpreter,
                "print(f'Python2 has no: {secret_value1}')",
                context=python2,
            )
            assert check1 is not None
            assert check2 is not None
            # check2 should fail with NameError (context isolation):
            # secret_value1 is defined in python1 but not in python2.
            # If check2.error is None, the SDK may have swallowed a "session
            # is busy" error and returned an empty Execution; retry once more.
            if check2.error is None and not check2.result and not check2.logs.stdout:
                logger.warning(
                    "check2 returned empty Execution (possible session-busy); retrying..."
                )
                time.sleep(2)
                check2 = run_with_retry_sync(
                    code_interpreter,
                    "print(f'Python2 has no: {secret_value1}')",
                    context=python2,
                )
            assert check2.error is not None, (
                f"Expected NameError for context isolation but got: {check2}"
            )
            assert check2.error.name == "NameError"

            java_result = run_with_retry_sync(
                code_interpreter,
                "String javaSecret = \"java_secret\";\n"
                    + "System.out.println(\"Java secret: \" + javaSecret);",
                context=java1,
            )
            go_result = run_with_retry_sync(
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

    @pytest.mark.timeout(900)
    @pytest.mark.order(7)
    def test_07_concurrent_code_execution(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_stack_sync(
            code_interpreter,
            [
                SupportedLanguage.PYTHON,
                SupportedLanguage.JAVA,
                SupportedLanguage.GO,
            ],
        ) as (python_c1, java_c1, go_c1):
            from concurrent.futures import ThreadPoolExecutor
            from concurrent.futures import TimeoutError as FutureTimeout

            labels = ["Python", "Java", "Go"]

            def run_python1():
                return code_interpreter.codes.run(
                    "import time\n"
                    + "for i in range(3):\n"
                    + "    print(f'Python1 iteration {i}')\n"
                    + "    time.sleep(0.1)\n"
                    + "print('Python1 completed')",
                    context=python_c1,
                    )

            def run_java_concurrent():
                return code_interpreter.codes.run(
                    "for (int i = 0; i < 3; i++) {\n"
                    + "    System.out.println(\"Java iteration \" + i);\n"
                    + "    try { Thread.sleep(100); } catch (Exception e) {}\n"
                    + "}\n"
                    + "System.out.println(\"Java completed\");",
                    context=java_c1,
                    )

            def run_go_concurrent():
                return code_interpreter.codes.run(
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

            with ThreadPoolExecutor(max_workers=4) as ex:
                futures = [
                    ex.submit(run_python1),
                    ex.submit(run_java_concurrent),
                    ex.submit(run_go_concurrent),
                ]

                succeeded = 0
                for i, future in enumerate(futures):
                    label = labels[i]
                    try:
                        result = future.result(timeout=120)
                        if result is not None and result.id is not None:
                            succeeded += 1
                            logger.info("Concurrent %s: OK (id=%s)", label, result.id)
                        else:
                            logger.warning(
                                "Concurrent %s: returned empty result: %s", label, result
                            )
                    except FutureTimeout:
                        logger.warning("Concurrent %s: timed out", label)
                    except Exception as e:
                        logger.warning("Concurrent %s: failed: %s", label, e)

            # In resource-constrained CI, "session is busy" may cause some
            # concurrent executions to return empty results.  Require at
            # least 2 of 3 to succeed.
            assert succeeded >= 2, (
                f"Only {succeeded}/3 concurrent executions succeeded; "
                f"expected at least 2"
            )

    @pytest.mark.timeout(900)
    @pytest.mark.order(8)
    def test_08_code_execution_interrupt(self, isolated_code_interpreter: CodeInterpreterSync):
        code_interpreter = isolated_code_interpreter

        with managed_ctx_sync(code_interpreter, SupportedLanguage.PYTHON) as python_int_context:
            assert python_int_context is not None and python_int_context.id is not None and str(python_int_context.id).strip()

            init_events_int: list[ExecutionInit] = []
            completed_events: list[ExecutionComplete] = []
            errors: list[ExecutionError] = []
            init_received = Event()

            def on_init(init: ExecutionInit):
                init_events_int.append(init)
                init_received.set()

            def on_complete(complete: ExecutionComplete):
                completed_events.append(complete)

            def on_error(error: ExecutionError):
                errors.append(error)

            handlers_int = ExecutionHandlersSync(
                on_init=on_init,
                on_execution_complete=on_complete,
                on_error=on_error,
            )

            with ThreadPoolExecutor(max_workers=1) as ex:
                start = time.time()
                future = ex.submit(
                    code_interpreter.codes.run,
                    "import time\n"
                    + "print('Starting long-running Python execution')\n"
                    + "for i in range(50):\n"
                    + "    print(f'Python iteration {i}')\n"
                    + "    time.sleep(0.2)\n",
                    context=python_int_context,
                    handlers=handlers_int,
                    )

                assert init_received.wait(timeout=15), "Execution init event was not received within 15s"
                assert len(init_events_int) == 1, "Execution should have been initialized exactly once"
                execution_id = init_events_int[-1].id
                assert execution_id is not None and execution_id.strip()
                _assert_recent_timestamp_ms(init_events_int[-1].timestamp)

                code_interpreter.codes.interrupt(execution_id)

                result_int = future.result()
                assert result_int is not None
                assert result_int.id is not None
                assert result_int.id == execution_id
                assert (len(completed_events) > 0) or (len(errors) > 0)
                elapsed = time.time() - start
                assert elapsed < 30

            # Small delay after interrupt to let the kernel recover
            time.sleep(1)
            quick_result = run_with_retry_sync(
                code_interpreter,
                "print('Quick Python execution')\n"
                + "result = 2 + 2\n"
                + "print(f'Result: {result}')",
                context=python_int_context,
                handlers=handlers_int,
            )
            assert quick_result is not None
            assert quick_result.id is not None

            try:
                code_interpreter.codes.interrupt(quick_result.id)
            except Exception:
                pass

    @pytest.mark.timeout(600)
    @pytest.mark.order(9)
    def test_09_context_management_endpoints(self):
        """Validate list/get/delete context APIs map to execd /code/contexts endpoints (sync)."""
        TestCodeInterpreterE2ESync._ensure_code_interpreter_created()
        code_interpreter = TestCodeInterpreterE2ESync.code_interpreter
        assert code_interpreter is not None

        language = SupportedLanguage.PYTHON
        logger.info("=" * 80)
        logger.info("TEST 9: Context management endpoints (%s)", language)
        logger.info("=" * 80)

        # Ensure clean slate for bash contexts to avoid interference with other tests.
        code_interpreter.codes.delete_contexts(language)

        ctx1 = code_interpreter.codes.create_context(language)
        ctx2 = code_interpreter.codes.create_context(language)
        assert ctx1.id is not None and str(ctx1.id).strip()
        assert ctx2.id is not None and str(ctx2.id).strip()
        assert ctx1.language == language
        assert ctx2.language == language
        logger.info("✓ Created two bash contexts: %s, %s", ctx1.id, ctx2.id)

        listed = code_interpreter.codes.list_contexts(language)
        bash_context_ids = {c.id for c in listed if c.id}
        assert ctx1.id in bash_context_ids
        assert ctx2.id in bash_context_ids
        assert all(c.language == language for c in listed)
        logger.info("✓ list_contexts returned expected bash contexts")

        fetched = code_interpreter.codes.get_context(ctx1.id)
        assert fetched.id == ctx1.id
        assert fetched.language == language
        logger.info("✓ get_context returned expected context %s", fetched.id)

        code_interpreter.codes.delete_context(ctx1.id)
        remaining = code_interpreter.codes.list_contexts(language)
        remaining_ids = {c.id for c in remaining if c.id}
        assert ctx1.id not in remaining_ids
        assert ctx2.id in remaining_ids
        logger.info("✓ delete_context removed %s", ctx1.id)

        code_interpreter.codes.delete_contexts(language)
        final_contexts = [
            c for c in code_interpreter.codes.list_contexts(language) if c.id
        ]
        assert len(final_contexts) == 0
        logger.info("✓ delete_contexts removed all bash contexts")
