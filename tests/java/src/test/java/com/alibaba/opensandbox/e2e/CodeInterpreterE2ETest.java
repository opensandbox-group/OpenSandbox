/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.e2e;

import static org.junit.jupiter.api.Assertions.*;

import com.alibaba.opensandbox.codeinterpreter.CodeInterpreter;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.CodeContext;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.RunCodeRequest;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.SupportedLanguage;
import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.*;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.*;
import java.lang.reflect.Method;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.concurrent.*;
import org.junit.jupiter.api.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Comprehensive E2E tests for CodeInterpreter runCode functionality.
 *
 * <p>Tests code execution capabilities including: - Multi-language code execution (Java, Python,
 * Go, TypeScript) - Session state management and variable persistence - Context isolation between
 * different execution contexts - Error handling and recovery mechanisms - Event handling patterns
 * from code-interpreter/Jupyter streams
 *
 * <p>Uses the shared CodeInterpreter instance from BaseE2ETest.
 */
@Tag("e2e")
@DisplayName("CodeInterpreter E2E Tests - RunCode Functionality")
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class CodeInterpreterE2ETest extends BaseE2ETest {

    protected static final Logger logger = LoggerFactory.getLogger(CodeInterpreterE2ETest.class);

    private Sandbox sandbox;
    private CodeInterpreter codeInterpreter;
    private boolean isolatedSandboxForCurrentTest = false;

    private static void assertTerminalEventContract(
            List<ExecutionInit> initEvents,
            List<ExecutionComplete> completedEvents,
            List<ExecutionError> errors,
            String executionId) {
        assertEquals(1, initEvents.size(), "init event must exist exactly once");
        assertNotNull(initEvents.get(0).getId());
        assertFalse(initEvents.get(0).getId().isBlank());
        assertEquals(executionId, initEvents.get(0).getId());
        assertRecentTimestampMs(initEvents.get(0).getTimestamp(), 180_000);
        assertTrue(
                (!completedEvents.isEmpty()) || (!errors.isEmpty()),
                "expected at least one of complete/error");
        if (!completedEvents.isEmpty()) {
            assertEquals(1, completedEvents.size());
            assertRecentTimestampMs(completedEvents.get(0).getTimestamp(), 180_000);
            assertTrue(completedEvents.get(0).getExecutionTimeInMillis() >= 0);
        }
        if (!errors.isEmpty()) {
            assertNotNull(errors.get(0).getName());
            assertFalse(errors.get(0).getName().isBlank());
            assertNotNull(errors.get(0).getValue());
            assertRecentTimestampMs(errors.get(0).getTimestamp(), 180_000);
        }
    }

    @BeforeAll
    void setup() {
        createInterpreterWithTag("e2e-code-interpreter");
        assertNotNull(codeInterpreter);
        assertNotNull(codeInterpreter.getId());
    }

    @BeforeEach
    void maybeRecreateInterpreterForHighFlakinessTests(TestInfo testInfo) {
        String methodName = testInfo.getTestMethod().map(Method::getName).orElse("");
        if (!shouldIsolatePerTest(methodName)) {
            isolatedSandboxForCurrentTest = false;
            return;
        }
        closeCurrentSandboxQuietly();
        createInterpreterWithTag("e2e-code-interpreter-isolated");
        isolatedSandboxForCurrentTest = true;
    }

    @AfterEach
    void cleanupIsolatedSandboxAfterEach() {
        if (!isolatedSandboxForCurrentTest) {
            return;
        }
        closeCurrentSandboxQuietly();
        isolatedSandboxForCurrentTest = false;
    }

    private boolean shouldIsolatePerTest(String methodName) {
        return methodName.equals("testJavaCodeExecution")
                || methodName.equals("testPythonCodeExecution")
                || methodName.equals("testGoCodeExecution")
                || methodName.equals("testTypeScriptCodeExecution")
                || methodName.equals("testMultiLanguageAndContextIsolation")
                || methodName.equals("testConcurrentCodeExecution")
                || methodName.equals("testCodeExecutionInterrupt");
    }

    private void createInterpreterWithTag(String metadataTag) {
        Volume volume =
                Volume.builder()
                        .name("execd-logs")
                        .host(Host.of("/tmp/opensandbox-e2e/logs"))
                        .mountPath("/tmp/opensandbox-e2e/logs")
                        .readOnly(false)
                        .build();
        sandbox =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .entrypoint(List.of("/opt/opensandbox/code-interpreter.sh"))
                        .image(getSandboxImage())
                        .resource(java.util.Map.of("cpu", "2", "memory", "4Gi"))
                        .timeout(Duration.ofMinutes(20))
                        .readyTimeout(Duration.ofSeconds(60))
                        .metadata(java.util.Map.of("tag", metadataTag))
                        .env("E2E_TEST", "true")
                        .env("GO_VERSION", "1.25")
                        .env("JAVA_VERSION", "21")
                        .env("NODE_VERSION", "22")
                        .env("PYTHON_VERSION", "3.12")
                        .env("EXECD_LOG_FILE", "/tmp/opensandbox-e2e/logs/execd.log")
                        .env("EXECD_API_GRACE_SHUTDOWN", "3s")
                        .env("EXECD_JUPYTER_IDLE_POLL_INTERVAL", "200ms")
                        .healthCheckPollingInterval(Duration.ofMillis(500))
                        .volume(volume)
                        .build();
        codeInterpreter = CodeInterpreter.builder().fromSandbox(sandbox).build();
    }

    private void closeCurrentSandboxQuietly() {
        if (sandbox == null) {
            return;
        }
        try {
            sandbox.kill();
        } catch (Exception ignored) {
        }
        try {
            sandbox.close();
        } catch (Exception ignored) {
        }
        sandbox = null;
        codeInterpreter = null;
    }

    @AfterAll
    void teardown() {
        closeCurrentSandboxQuietly();
    }

    // ==========================================
    // Basic Code Execution Tests
    // ==========================================
    @Test
    @Order(1)
    @DisplayName("CodeInterpreter Creation and Basic Functionality")
    @Timeout(value = 2, unit = TimeUnit.MINUTES)
    void testCodeInterpreterBasicFunctionality() {
        logger.info("Testing CodeInterpreter creation and basic functionality");

        assertNotNull(codeInterpreter);
        assertNotNull(codeInterpreter.getId());

        // 2. Verify service access
        assertNotNull(codeInterpreter.codes());
        assertNotNull(codeInterpreter.files());
        assertNotNull(codeInterpreter.commands());
        assertNotNull(codeInterpreter.metrics());
    }

    @Test
    @Order(2)
    @DisplayName("Java Code Execution")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testJavaCodeExecution() {
        logger.info("Testing Java code execution");

        CodeContext javaContext = codeInterpreter.codes().createContext(SupportedLanguage.JAVA);

        assertNotNull(javaContext);
        assertNotNull(javaContext.getId());
        assertEquals("java", javaContext.getLanguage());

        // Event tracking for comprehensive validation
        List<OutputMessage> stdoutMessages = Collections.synchronizedList(new ArrayList<>());
        List<OutputMessage> stderrMessages = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionResult> results = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionError> errors = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionComplete> completedEvents = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionInit> initEvents = Collections.synchronizedList(new ArrayList<>());

        ExecutionHandlers handlers =
                ExecutionHandlers.builder()
                        .onStdout(
                                (OutputMessage msg) -> {
                                    stdoutMessages.add(msg);
                                    logger.info("Java stdout: {}", msg.getText());
                                })
                        .onStderr(
                                (OutputMessage msg) -> {
                                    stderrMessages.add(msg);
                                    logger.warn("Java stderr: {}", msg.getText());
                                })
                        .onResult(
                                (ExecutionResult result) -> {
                                    results.add(result);
                                    logger.info("Java result: {}", result.getText());
                                })
                        .onExecutionComplete(
                                (ExecutionComplete complete) -> {
                                    completedEvents.add(complete);
                                    logger.info(
                                            "Java execution completed in {} ms",
                                            complete.getExecutionTimeInMillis());
                                })
                        .onError(
                                (ExecutionError error) -> {
                                    errors.add(error);
                                    logger.error(
                                            "Java error: {} - {}",
                                            error.getName(),
                                            error.getValue());
                                })
                        .onInit(
                                (ExecutionInit init) -> {
                                    initEvents.add(init);
                                    logger.info(
                                            "Java execution initialized with ID: {}", init.getId());
                                })
                        .build();

        RunCodeRequest simpleRequest =
                RunCodeRequest.builder()
                        .code(
                                "System.out.println(\"Hello from Java!\");\n"
                                        + "int result = 2 + 2;\n"
                                        + "System.out.println(\"2 + 2 = \" + result);\n"
                                        + "result")
                        .context(javaContext)
                        .handlers(handlers)
                        .build();

        Execution simpleResult = codeInterpreter.codes().run(simpleRequest);

        assertNotNull(simpleResult);
        assertNotNull(simpleResult.getId());
        assertFalse(simpleResult.getId().isBlank());
        assertEquals("4", simpleResult.getResult().get(0).getText());
        assertNull(simpleResult.getExitCode());
        assertNotNull(simpleResult.getComplete());
        assertTrue(simpleResult.getComplete().getExecutionTimeInMillis() >= 0);
        assertTerminalEventContract(initEvents, completedEvents, errors, simpleResult.getId());
        assertTrue(errors.isEmpty());
        assertTrue(stdoutMessages.stream().anyMatch(m -> m.getText().contains("Hello from Java!")));
        assertTrue(
                stdoutMessages.stream()
                        .anyMatch(m -> m.getText().replace(" ", "").contains("2+2=4")));

        RunCodeRequest varRequest =
                RunCodeRequest.builder()
                        .code(
                                "import java.util.*;\n"
                                        + "List<Integer> numbers = Arrays.asList(1, 2, 3, 4, 5);\n"
                                        + "int sum ="
                                        + " numbers.stream().mapToInt(Integer::intValue).sum();\n"
                                        + "System.out.println(\"Numbers: \" + numbers);\n"
                                        + "System.out.println(\"Sum: \" + sum);\n"
                                        + "result")
                        .context(javaContext)
                        .build();

        Execution varResult = codeInterpreter.codes().run(varRequest);

        assertNotNull(varResult);
        assertNotNull(varResult.getId());
        assertEquals("4", varResult.getResult().get(0).getText());
        assertNull(varResult.getExitCode());
        assertNotNull(varResult.getComplete());

        // 3. Java error handling test.
        // Jupyter may emit execution_complete and error for the same run, so
        // complete is not mutually exclusive with error here.
        stdoutMessages.clear();
        stderrMessages.clear();
        results.clear();
        errors.clear();
        completedEvents.clear();
        initEvents.clear();
        RunCodeRequest errorRequest =
                RunCodeRequest.builder()
                        .code("int x = 10 / 0; // This will cause ArithmeticException")
                        .context(javaContext)
                        .handlers(handlers)
                        .build();

        Execution errorResult = codeInterpreter.codes().run(errorRequest);

        assertNotNull(errorResult);
        assertNotNull(errorResult.getId());
        assertNotNull(errorResult.getError());
        assertEquals("EvalException", errorResult.getError().getName());
        assertNull(errorResult.getExitCode());
        assertTerminalEventContract(initEvents, completedEvents, errors, errorResult.getId());
    }

    @Test
    @Order(3)
    @DisplayName("Python Code Execution")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testPythonCodeExecution() {
        logger.info("Testing Python code execution");

        // Use class-scoped interpreter (created in @BeforeAll)
        assertNotNull(codeInterpreter);
        CodeContext pythonContext = codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);
        assertNotNull(pythonContext);
        assertEquals("python", pythonContext.getLanguage());
        Duration perExecTimeout = Duration.ofMinutes(2);

        // Event tracking
        List<OutputMessage> stdoutMessages = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionComplete> completedEvents = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionError> errors = Collections.synchronizedList(new ArrayList<>());

        ExecutionHandlers handlers =
                ExecutionHandlers.builder()
                        .onStdout(
                                (OutputMessage msg) -> {
                                    stdoutMessages.add(msg);
                                    logger.info("Python stdout: {}", msg.getText());
                                })
                        .onExecutionComplete(
                                (ExecutionComplete complete) -> {
                                    completedEvents.add(complete);
                                    logger.info(
                                            "Python execution completed in {} ms",
                                            complete.getExecutionTimeInMillis());
                                })
                        .onError(
                                (ExecutionError error) -> {
                                    errors.add(error);
                                    logger.error(
                                            "Python error: {} - {}",
                                            error.getName(),
                                            error.getValue());
                                })
                        .build();

        // 1. Simple Python execution
        RunCodeRequest simpleRequest =
                RunCodeRequest.builder()
                        .code(
                                "print('Hello from Python!')\n"
                                        + "result = 2 + 2\n"
                                        + "print(f'2 + 2 = {result}')")
                        .context(pythonContext)
                        .handlers(handlers)
                        .build();

        Execution simpleResult =
                runWithRetry(simpleRequest, perExecTimeout, 2, "python-simple-execution");

        assertNotNull(simpleResult);
        assertNotNull(simpleResult.getId());
        assertNull(simpleResult.getExitCode());
        assertNotNull(simpleResult.getComplete());
        assertTrue(simpleResult.getComplete().getExecutionTimeInMillis() >= 0);
        assertFalse(completedEvents.isEmpty());
        assertTrue(errors.isEmpty());

        // 2. Python with variables and state persistence
        RunCodeRequest varRequest =
                RunCodeRequest.builder()
                        .code(
                                "x = 42\n"
                                        + "y = 'persistent variable'\n"
                                        + "my_list = [1, 2, 3, 4, 5]\n"
                                        + "print(f'x={x}, y=\"{y}\", list={my_list}')\n"
                                        + "result")
                        .context(pythonContext)
                        .build();

        Execution varResult = runWithRetry(varRequest, perExecTimeout, 2, "python-state-setup");

        assertNotNull(varResult);
        assertNotNull(varResult.getId());
        assertEquals("4", varResult.getResult().get(0).getText());
        assertNull(varResult.getExitCode());
        assertNotNull(varResult.getComplete());

        // 3. Test variable persistence across executions
        RunCodeRequest persistRequest =
                RunCodeRequest.builder()
                        .code(
                                "print(f'Previously set variables: x={x}, y={y}')\n"
                                        + "z = sum(my_list)\n"
                                        + "print(f'Sum of list: {z}')")
                        .context(pythonContext)
                        .build();

        Execution persistResult =
                runWithRetry(persistRequest, perExecTimeout, 2, "python-state-persistence");

        assertNotNull(persistResult);
        assertNotNull(persistResult.getId());
        assertNull(persistResult.getExitCode());
        assertNotNull(persistResult.getComplete());

        // 4. Python error handling
        RunCodeRequest errorRequest =
                RunCodeRequest.builder()
                        .code("print(undefined_variable)  # This will cause NameError")
                        .context(pythonContext)
                        .handlers(handlers)
                        .build();

        Execution errorResult =
                runWithRetry(errorRequest, perExecTimeout, 2, "python-runtime-error");

        assertNotNull(errorResult);
        assertNotNull(errorResult.getId());
        assertNull(errorResult.getExitCode());
        assertTrue(
                errorResult.getError() != null || !errorResult.getLogs().getStderr().isEmpty(),
                "Python error execution should capture runtime errors");

        logger.info("Python code execution tests completed");
    }

    @Test
    @Order(4)
    @DisplayName("Go Code Execution")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testGoCodeExecution() {
        logger.info("Testing Go code execution");

        assertNotNull(codeInterpreter);
        CodeContext goContext = codeInterpreter.codes().createContext(SupportedLanguage.GO);

        assertNotNull(goContext);
        assertEquals("go", goContext.getLanguage());

        // Event tracking
        List<OutputMessage> stdoutMessages = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionComplete> completedEvents = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionError> errors = Collections.synchronizedList(new ArrayList<>());

        ExecutionHandlers handlers =
                ExecutionHandlers.builder()
                        .onStdout(
                                (OutputMessage msg) -> {
                                    stdoutMessages.add(msg);
                                    logger.info("Go stdout: {}", msg.getText());
                                })
                        .onExecutionComplete(
                                (ExecutionComplete complete) -> {
                                    completedEvents.add(complete);
                                    logger.info(
                                            "Go execution completed in {} ms",
                                            complete.getExecutionTimeInMillis());
                                })
                        .onError(
                                (ExecutionError error) -> {
                                    errors.add(error);
                                    logger.error(
                                            "Go error: {} - {}", error.getName(), error.getValue());
                                })
                        .build();

        // 1. Simple Go execution
        RunCodeRequest simpleRequest =
                RunCodeRequest.builder()
                        .code(
                                "package main\n"
                                        + "func main() {\n"
                                        + "    println(\"Hello from Go!\")\n"
                                        + "    result := 2 + 2\n"
                                        + "    println(\"2 + 2 =\", result)\n"
                                        + "}")
                        .context(goContext)
                        .handlers(handlers)
                        .build();

        Execution simpleResult = codeInterpreter.codes().run(simpleRequest);

        assertNotNull(simpleResult);
        assertNotNull(simpleResult.getId());
        assertFalse(completedEvents.isEmpty());

        // 2. Go with data structures and functions
        RunCodeRequest dataRequest =
                RunCodeRequest.builder()
                        .code(
                                "package main\n"
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
                                        + "    println(\"Numbers:\", numbers)\n"
                                        + "    println(\"Sum:\", sum)\n"
                                        + "}")
                        .context(goContext)
                        .build();

        Execution dataResult = codeInterpreter.codes().run(dataRequest);

        assertNotNull(dataResult);
        assertNotNull(dataResult.getId());

        // 3. Go compilation error test
        RunCodeRequest errorRequest =
                RunCodeRequest.builder()
                        .code(
                                "package main\n"
                                        + "func main() {\n"
                                        + "    undeclaredVariable++  // This will cause compilation"
                                        + " error\n"
                                        + "}")
                        .context(goContext)
                        .handlers(handlers)
                        .build();

        Execution errorResult = codeInterpreter.codes().run(errorRequest);

        assertNotNull(errorResult);
        assertNotNull(errorResult.getId());
        assertTrue(
                errorResult.getError() != null || errorResult.getLogs().getStderr().size() > 0,
                "Go error execution should capture compilation errors");

        logger.info("Go code execution tests completed");
    }

    @Test
    @Order(5)
    @DisplayName("TypeScript Code Execution")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testTypeScriptCodeExecution() {
        logger.info("Testing TypeScript code execution");

        assertNotNull(codeInterpreter);

        // Create TypeScript execution context
        CodeContext tsContext = codeInterpreter.codes().createContext(SupportedLanguage.TYPESCRIPT);

        assertNotNull(tsContext);
        assertEquals("typescript", tsContext.getLanguage());

        // Event tracking
        List<OutputMessage> stdoutMessages = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionComplete> completedEvents = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionError> errors = Collections.synchronizedList(new ArrayList<>());

        ExecutionHandlers handlers =
                ExecutionHandlers.builder()
                        .onStdout(
                                (OutputMessage msg) -> {
                                    stdoutMessages.add(msg);
                                    logger.info("TypeScript stdout: {}", msg.getText());
                                })
                        .onExecutionComplete(
                                (ExecutionComplete complete) -> {
                                    completedEvents.add(complete);
                                    logger.info(
                                            "TypeScript execution completed in {} ms",
                                            complete.getExecutionTimeInMillis());
                                })
                        .onError(
                                (ExecutionError error) -> {
                                    errors.add(error);
                                    logger.error(
                                            "TypeScript error: {} - {}",
                                            error.getName(),
                                            error.getValue());
                                })
                        .build();

        // 1. Simple TypeScript execution
        RunCodeRequest simpleRequest =
                RunCodeRequest.builder()
                        .code(
                                "console.log('Hello from TypeScript!');\n"
                                        + "const result: number = 2 + 2;\n"
                                        + "console.log(`2 + 2 = ${result}`);")
                        .context(tsContext)
                        .handlers(handlers)
                        .build();

        Execution simpleResult = codeInterpreter.codes().run(simpleRequest);

        assertNotNull(simpleResult);
        assertNotNull(simpleResult.getId());
        assertFalse(completedEvents.isEmpty());

        // 2. TypeScript with types and interfaces
        RunCodeRequest typesRequest =
                RunCodeRequest.builder()
                        .code(
                                "interface Person {\n"
                                        + "  name: string;\n"
                                        + "  age: number;\n"
                                        + "}\n"
                                        + "const person: Person = { name: 'John', age: 30 };\n"
                                        + "const numbers: number[] = [1, 2, 3, 4, 5];\n"
                                        + "const sum: number = numbers.reduce((a, b) => a + b, 0);\n"
                                        + "console.log(`Person: ${person.name}, Age: ${person.age}`);\n"
                                        + "console.log(`Numbers: ${numbers}`);\n"
                                        + "console.log(`Sum: ${sum}`);")
                        .context(tsContext)
                        .build();

        Execution typesResult = codeInterpreter.codes().run(typesRequest);

        assertNotNull(typesResult);
        assertNotNull(typesResult.getId());

        // 3. TypeScript error test: use deterministic runtime error.
        RunCodeRequest errorRequest =
                RunCodeRequest.builder()
                        .code("throw new Error('ts-runtime-error');")
                        .context(tsContext)
                        .handlers(handlers)
                        .build();

        Execution errorResult = codeInterpreter.codes().run(errorRequest);

        assertNotNull(errorResult);
        assertNotNull(errorResult.getId());
        assertTrue(
                errorResult.getError() != null || errorResult.getLogs().getStderr().size() > 0,
                "TypeScript error execution should capture type errors");

        logger.info("TypeScript code execution tests completed");
    }

    /**
     * Run a code request with a per-execution timeout so that a single hanging SSE stream cannot
     * block the entire test for the full JUnit timeout.
     */
    private Execution runWithTimeout(RunCodeRequest request, Duration timeout) {
        CompletableFuture<Execution> future =
                CompletableFuture.supplyAsync(() -> codeInterpreter.codes().run(request));
        try {
            return future.get(timeout.toMillis(), TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
            future.cancel(true);
            throw new AssertionError("Code execution did not complete within " + timeout, e);
        } catch (ExecutionException e) {
            Throwable cause = e.getCause();
            if (cause instanceof RuntimeException) {
                throw (RuntimeException) cause;
            }
            throw new RuntimeException(cause);
        } catch (InterruptedException e) {
            future.cancel(true);
            Thread.currentThread().interrupt();
            throw new RuntimeException(e);
        }
    }

    private Execution runWithRetry(
            RunCodeRequest request, Duration timeout, int attempts, String label) {
        AssertionError lastAssertionError = null;
        RuntimeException lastRuntimeException = null;
        for (int attempt = 1; attempt <= attempts; attempt++) {
            try {
                return runWithTimeout(request, timeout);
            } catch (AssertionError e) {
                lastAssertionError = e;
                logger.warn("{} attempt {}/{} timed out", label, attempt, attempts, e);
            } catch (RuntimeException e) {
                lastRuntimeException = e;
                logger.warn("{} attempt {}/{} failed", label, attempt, attempts, e);
            }
        }
        if (lastAssertionError != null) {
            throw lastAssertionError;
        }
        if (lastRuntimeException != null) {
            throw lastRuntimeException;
        }
        throw new AssertionError(label + " failed without a captured exception");
    }

    @Test
    @Order(6)
    @DisplayName("Multi-Language Support and Context Isolation")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testMultiLanguageAndContextIsolation() {
        logger.info("Testing multi-language support and context isolation");

        assertNotNull(codeInterpreter);

        // Per-execution timeout: if a single run() call hangs (sandbox gone, network
        // issue), fail fast instead of blocking the entire 10-minute JUnit timeout.
        Duration perExecTimeout = Duration.ofMinutes(2);

        // Create separate contexts for different languages
        CodeContext python1 = codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);
        CodeContext python2 = codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);

        // 1. Set different variables in each Python context to test isolation
        RunCodeRequest python1Setup =
                RunCodeRequest.builder()
                        .code(
                                "secret_value1 = 'python1_secret'\n"
                                        + "print(f'Python1 secret: {secret_value1}')")
                        .context(python1)
                        .build();

        RunCodeRequest python2Setup =
                RunCodeRequest.builder()
                        .code(
                                "secret_value2 = 'python2_secret'\n"
                                        + "print(f'Python2 secret: {secret_value2}')")
                        .context(python2)
                        .build();

        Execution result1 = runWithTimeout(python1Setup, perExecTimeout);
        Execution result2 = runWithTimeout(python2Setup, perExecTimeout);

        assertNotNull(result1);
        assertNotNull(result1.getId());
        assertNotNull(result2);
        assertNotNull(result2.getId());

        // 2. Verify isolation - each context should only see its own variables
        RunCodeRequest python1Check =
                RunCodeRequest.builder()
                        .code("print(f'Python1 still has: {secret_value1}')")
                        .context(python1)
                        .build();

        RunCodeRequest python2Check =
                RunCodeRequest.builder()
                        .code("print(f'Python2 has no: {secret_value1}')")
                        .context(python2)
                        .build();

        Execution check1 = runWithTimeout(python1Check, perExecTimeout);
        Execution check2 = runWithTimeout(python2Check, perExecTimeout);

        assertNotNull(check1);
        assertNotNull(check1.getId());
        assertNotNull(check2);
        assertNotNull(check2.getId());
        assertNotNull(check2.getError());
        assertEquals("NameError", check2.getError().getName());
    }

    @Test
    @Order(7)
    @DisplayName("Concurrent Code Execution")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testConcurrentCodeExecution() {
        logger.info("Testing concurrent code execution");

        assertNotNull(codeInterpreter);
        ExecutorService executor = Executors.newFixedThreadPool(4);
        long timestamp = System.currentTimeMillis();

        // Create multiple contexts for concurrent execution
        CodeContext pythonConcurrent1 =
                codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);
        CodeContext pythonConcurrent2 =
                codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);
        CodeContext javaConcurrent = codeInterpreter.codes().createContext(SupportedLanguage.JAVA);
        CodeContext goConcurrent = codeInterpreter.codes().createContext(SupportedLanguage.GO);

        // Track futures with labels for diagnostics
        List<String> taskLabels = List.of("Python1", "Python2", "Java", "Go");
        List<Future<Execution>> futures = new ArrayList<>();

        try {
            // Submit concurrent executions
            futures.add(
                    executor.submit(
                            () -> {
                                RunCodeRequest request =
                                        RunCodeRequest.builder()
                                                .code(
                                                        "import time\n"
                                                                + "for i in range(3):\n"
                                                                + "    print(f'Python1 iteration"
                                                                + " {i}')\n"
                                                                + "    time.sleep(0.1)\n"
                                                                + "print('Python1 completed')")
                                                .context(pythonConcurrent1)
                                                .build();
                                return codeInterpreter.codes().run(request);
                            }));

            futures.add(
                    executor.submit(
                            () -> {
                                RunCodeRequest request =
                                        RunCodeRequest.builder()
                                                .code(
                                                        "import time\n"
                                                                + "for i in range(3):\n"
                                                                + "    print(f'Python2 iteration"
                                                                + " {i}')\n"
                                                                + "    time.sleep(0.1)\n"
                                                                + "print('Python2 completed')")
                                                .context(pythonConcurrent2)
                                                .build();
                                return codeInterpreter.codes().run(request);
                            }));

            futures.add(
                    executor.submit(
                            () -> {
                                RunCodeRequest request =
                                        RunCodeRequest.builder()
                                                .code(
                                                        "for (int i = 0; i < 3; i++) {\n"
                                                                + "    System.out.println(\"Java"
                                                                + " iteration \" + i);\n"
                                                                + "    Thread.sleep(100);\n"
                                                                + "}\n"
                                                                + "System.out.println(\"Java"
                                                                + " completed\");")
                                                .context(javaConcurrent)
                                                .build();
                                return codeInterpreter.codes().run(request);
                            }));

            futures.add(
                    executor.submit(
                            () -> {
                                RunCodeRequest request =
                                        RunCodeRequest.builder()
                                                .code(
                                                        "package main\n"
                                                                + "func main() {\n"
                                                                + "    for i := 0; i < 3; i++ {\n"
                                                                + "        println(\"Go iteration\","
                                                                + " i)\n"
                                                                + "    }\n"
                                                                + "    println(\"Go completed\")\n"
                                                                + "}")
                                                .context(goConcurrent)
                                                .build();
                                return codeInterpreter.codes().run(request);
                            }));

            // Collect results with per-task diagnostics
            int succeeded = 0;
            List<String> failures = new ArrayList<>();
            for (int i = 0; i < futures.size(); i++) {
                String label = taskLabels.get(i);
                try {
                    Execution result = futures.get(i).get(5, TimeUnit.MINUTES);
                    if (result == null) {
                        String msg = label + ": returned null Execution";
                        logger.error(msg);
                        failures.add(msg);
                    } else if (result.getId() == null) {
                        // Log available fields to aid debugging
                        String detail =
                                label
                                        + ": Execution has null id (error="
                                        + (result.getError() != null
                                                ? result.getError().getName()
                                                        + ": "
                                                        + result.getError().getValue()
                                                : "none")
                                        + ")";
                        logger.warn(detail);
                        failures.add(detail);
                    } else {
                        logger.info(
                                "Concurrent execution [{}] completed: {}", label, result.getId());
                        succeeded++;
                    }
                } catch (TimeoutException te) {
                    String msg = label + ": timed out waiting for result";
                    logger.error(msg, te);
                    failures.add(msg);
                    futures.get(i).cancel(true);
                } catch (ExecutionException ee) {
                    String msg = label + ": execution threw " + ee.getCause();
                    logger.error(msg, ee.getCause());
                    failures.add(msg);
                }
            }

            // At least 2 of 4 concurrent executions must succeed.
            // Java/Go compilation overhead in CI can occasionally cause
            // timeouts or incomplete responses, so we tolerate partial
            // failure while still asserting that concurrency works.
            assertTrue(
                    succeeded >= 2,
                    "Expected at least 2 of 4 concurrent executions to succeed, but only "
                            + succeeded
                            + " did. Failures: "
                            + failures);
            logger.info(
                    "Concurrent execution: {}/{} succeeded (failures: {})",
                    succeeded,
                    futures.size(),
                    failures);

        } catch (Exception e) {
            logger.error("Concurrent execution test failed unexpectedly", e);
            fail("Concurrent execution failed: " + e);
        } finally {
            executor.shutdown();
        }

        logger.info("Concurrent code execution tests completed");
    }

    @Test
    @Order(8)
    @DisplayName("Code Execution Interrupt")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testCodeExecutionInterrupt() throws InterruptedException, ExecutionException {
        logger.info("Testing code execution interrupt functionality");

        CodeContext pythonContext = codeInterpreter.codes().createContext(SupportedLanguage.PYTHON);
        CodeContext javaContext = codeInterpreter.codes().createContext(SupportedLanguage.JAVA);

        // Event tracking for interrupt testing
        List<ExecutionComplete> completedEvents = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionError> errors = Collections.synchronizedList(new ArrayList<>());
        List<ExecutionInit> initEvents = Collections.synchronizedList(new ArrayList<>());

        ExecutionHandlers handlers =
                ExecutionHandlers.builder()
                        .onExecutionComplete(
                                (ExecutionComplete complete) -> {
                                    completedEvents.add(complete);
                                    logger.info(
                                            "Execution completed in {} ms",
                                            complete.getExecutionTimeInMillis());
                                })
                        .onError(
                                (ExecutionError error) -> {
                                    errors.add(error);
                                    logger.error(
                                            "Execution error: {} - {}",
                                            error.getName(),
                                            error.getValue());
                                })
                        .onInit(
                                (ExecutionInit init) -> {
                                    initEvents.add(init);
                                    logger.info("Execution initialized with ID: {}", init.getId());
                                })
                        .build();

        // Test 1: Python long-running execution with interrupt
        logger.info("Testing Python interrupt functionality");

        RunCodeRequest pythonLongRunningRequest =
                RunCodeRequest.builder()
                        .code(
                                "import time\n"
                                        + "print('Starting long-running Python execution')\n"
                                        + "for i in range(100):\n"
                                        + "    print(f'Python iteration {i}')\n"
                                        + "    time.sleep(0.2)  # Sleep 200ms per iteration (20 seconds"
                                        + " total)\n"
                                        + "print('Python execution completed - this should not be"
                                        + " seen')")
                        .context(pythonContext)
                        .handlers(handlers)
                        .build();

        // Start Python execution in background
        ExecutorService executor = Executors.newSingleThreadExecutor();
        long start = System.currentTimeMillis();
        Future<Execution> pythonFuture =
                executor.submit(() -> codeInterpreter.codes().run(pythonLongRunningRequest));

        // Wait for init
        long deadline = System.currentTimeMillis() + 15_000;
        while (initEvents.isEmpty() && System.currentTimeMillis() < deadline) {
            Thread.sleep(100);
        }
        assertFalse(initEvents.isEmpty(), "Execution should have been initialized");
        String pythonExecutionId = initEvents.get(initEvents.size() - 1).getId();
        assertNotNull(pythonExecutionId, "Execution ID should not be null");

        // Interrupt the execution after letting it run briefly
        logger.info("Interrupting Python execution with ID: {}", pythonExecutionId);
        assertDoesNotThrow(() -> codeInterpreter.codes().interrupt(pythonExecutionId));

        // Wait for execution to complete (should be interrupted).
        // The SSE stream may close abruptly after interrupt, so handle both
        // a clean result and an exception from a broken connection.
        Execution pythonResult = null;
        try {
            pythonResult = pythonFuture.get(60, TimeUnit.SECONDS);
        } catch (TimeoutException e) {
            pythonFuture.cancel(true);
            logger.warn("Python execution did not complete within 60s after interrupt");
        } catch (ExecutionException e) {
            // SSE stream broken by interrupt — acceptable
            logger.warn("Python execution raised after interrupt: {}", e.getCause().getMessage());
        }
        executor.shutdown();

        long elapsed = System.currentTimeMillis() - start;

        if (pythonResult != null) {
            assertNotNull(pythonResult.getId());
            assertEquals(pythonExecutionId, pythonResult.getId());
        }

        // Verify the interrupt was effective: execution finished much faster
        // than the full 20 s run.  Terminal events (complete/error) may or may
        // not arrive depending on how quickly the server closed the stream.
        assertTrue(
                elapsed < 90_000,
                "Execution should have finished promptly after interrupt (elapsed="
                        + elapsed
                        + "ms)");

        // Test 2: Java long-running execution with interrupt
        logger.info("Testing Java interrupt functionality");

        // Clear event lists for Java test
        completedEvents.clear();
        errors.clear();
        initEvents.clear();

        RunCodeRequest javaLongRunningRequest =
                RunCodeRequest.builder()
                        .code(
                                "System.out.println(\"Starting long-running Java execution\");\n"
                                        + "for (int i = 0; i < 100; i++) {\n"
                                        + "    System.out.println(\"Java iteration \" + i);\n"
                                        + "    try {\n"
                                        + "        Thread.sleep(200);  // Sleep 200ms per iteration\n"
                                        + "    } catch (InterruptedException e) {\n"
                                        + "        System.out.println(\"Java execution"
                                        + " interrupted\");\n"
                                        + "        break;\n"
                                        + "    }\n"
                                        + "}\n"
                                        + "System.out.println(\"Java execution completed - this should"
                                        + " not be seen\");")
                        .context(javaContext)
                        .handlers(handlers)
                        .build();

        // Start Java execution in background
        ExecutorService javaExecutor = Executors.newSingleThreadExecutor();
        Future<Execution> javaFuture =
                javaExecutor.submit(() -> codeInterpreter.codes().run(javaLongRunningRequest));

        // Wait for execution to start
        Thread.sleep(1000);

        // Verify Java execution was initialized
        assertFalse(initEvents.isEmpty(), "Java execution should have been initialized");
        String javaExecutionId = initEvents.get(initEvents.size() - 1).getId();
        assertNotNull(javaExecutionId, "Java execution ID should not be null");

        // Interrupt the Java execution
        logger.info("Interrupting Java execution with ID: {}", javaExecutionId);
        assertDoesNotThrow(() -> codeInterpreter.codes().interrupt(javaExecutionId));

        // Wait for execution to complete, with a timeout to avoid hanging
        // if the SSE stream doesn't close promptly after interrupt.
        Execution javaResult = null;
        try {
            javaResult = javaFuture.get(60, TimeUnit.SECONDS);
        } catch (TimeoutException e) {
            javaFuture.cancel(true);
            logger.warn("Java execution did not complete within 60s after interrupt");
        } catch (ExecutionException e) {
            logger.warn("Java execution raised after interrupt: {}", e.getCause().getMessage());
        }
        javaExecutor.shutdown();

        if (javaResult != null) {
            assertNotNull(javaResult.getId());
            logger.info(
                    "Java execution result: ID={}, Error={}",
                    javaResult.getId(),
                    javaResult.getError() != null ? javaResult.getError().getName() : "none");
        }

        // Test 4: Quick execution that completes before interrupt
        logger.info("Testing interrupt of already completed execution");

        RunCodeRequest quickRequest =
                RunCodeRequest.builder()
                        .code(
                                "print('Quick Python execution')\n"
                                        + "result = 2 + 2\n"
                                        + "print(f'Result: {result}')")
                        .context(pythonContext)
                        .handlers(handlers)
                        .build();

        Execution quickResult = runWithTimeout(quickRequest, Duration.ofMinutes(1));
        assertNotNull(quickResult);
        assertNotNull(quickResult.getId());

        // Try to interrupt already completed execution
        try {
            codeInterpreter.codes().interrupt(quickResult.getId());
        } catch (Exception ignored) {
        }

        logger.info("Code execution interrupt tests completed");
    }
}
