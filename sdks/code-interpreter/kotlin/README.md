# Alibaba Code Interpreter SDK for Kotlin

English | [中文](README_zh.md)

A powerful Kotlin SDK for executing code in secure, isolated sandboxes. This SDK provides a high-level API for running Python, Java, Go, TypeScript, and other languages safely, with support for code execution contexts.

## Prerequisites

This SDK requires a specific Docker image containing the Code Interpreter runtime environment. You must use the `opensandbox/code-interpreter` image (or a derivative) which includes pre-installed runtimes for Python, Java, Go, Node.js, etc.

## Installation

### Gradle (Kotlin DSL)

```kotlin
dependencies {
    implementation("com.alibaba.opensandbox:code-interpreter:{latest_version}")
}
```

### Maven

```xml
<dependency>
    <groupId>com.alibaba.opensandbox</groupId>
    <artifactId>code-interpreter</artifactId>
    <version>{latest_version}</version>
</dependency>
```

## Quick Start

The following example demonstrates how to initialize the client with a specific Python version and execute a simple script.

```java
import com.alibaba.opensandbox.codeinterpreter.CodeInterpreter;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.CodeContext;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.RunCodeRequest;
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.SupportedLanguage;
import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;

public class QuickStart {
    public static void main(String[] args) {
        // 1. Configure connection
        ConnectionConfig config = ConnectionConfig.builder()
            .domain("api.opensandbox.io")
            .apiKey("your-api-key")
            .build();

        // 2. Create a Sandbox with specific runtime configuration
        // Note: You must use the code-interpreter image
        // Use try-with-resources to ensure sandbox is closed
        try (Sandbox sandbox = Sandbox.builder()
                .connectionConfig(config)
                .image("opensandbox/code-interpreter:v1.0.2")
                .entrypoint("/opt/opensandbox/code-interpreter.sh")
                .env("PYTHON_VERSION", "3.11") // Select specific language version
                .build()) {

            // 3. Create CodeInterpreter wrapper
            CodeInterpreter interpreter = CodeInterpreter.builder()
                .fromSandbox(sandbox)
                .build();

            // 4. Create an execution context (Python)
            CodeContext context = interpreter.codes().createContext(SupportedLanguage.PYTHON);

            // 5. Run code
            Execution result = interpreter.codes().run(
                RunCodeRequest.builder()
                    .code("import sys; print(f'Running on Python {sys.version}')")
                    .context(context)
                    .build()
            );

            // 6. Print output
            if (!result.getLogs().getStdout().isEmpty()) {
                System.out.println(result.getLogs().getStdout().get(0).getText());
            }

            // 7. Cleanup
            // Note: kill() terminates the remote instance; close() (auto-called) cleans up local resources
            sandbox.kill();
        } catch (SandboxException e) {
            // Handle Sandbox specific exceptions
            System.err.println("Sandbox Error: [" + e.getError().getCode() + "] " + e.getError().getMessage());
        } catch (Exception e) {
            e.printStackTrace();
        }
    }
}
```

## Runtime Configuration

### Docker Image

The Code Interpreter SDK relies on a specialized environment. Ensure your sandbox provider has the `opensandbox/code-interpreter` image available.

For detailed information about supported languages and versions, please refer to the [Environment Documentation](../../../sandboxes/code-interpreter/README.md).

### Troubleshooting: `pip: command not found`

If you are using the Kotlin/Java client and run shell commands such as
`pip install pandas`, make sure the sandbox is created from the
`opensandbox/code-interpreter` image (or a derivative) and uses the
Code Interpreter entrypoint:

```java
Sandbox sandbox = Sandbox.builder()
    .image("opensandbox/code-interpreter:v1.0.2")
    .entrypoint("/opt/opensandbox/code-interpreter.sh")
    .env("PYTHON_VERSION", "3.11")
    .build();
```

The plain Sandbox SDK can also talk to generic sandbox images, but those
images are not guaranteed to include Python or `pip`. In that case,
commands such as `pip install ...` will fail with `pip: command not found`.
Use the package manager that matches the image you launched, or switch to the
Code Interpreter image when you need Python package installation/runtime
behavior.

### Language Version Selection

You can specify the desired version of a programming language by setting the corresponding environment variable when building the `Sandbox`.

| Language | Environment Variable | Example Value | Default (if unset) |
| -------- | -------------------- | ------------- | ------------------ |
| Python   | `PYTHON_VERSION`     | `3.11`        | Image default      |
| Java     | `JAVA_VERSION`       | `17`          | Image default      |
| Node.js  | `NODE_VERSION`       | `20`          | Image default      |
| Go       | `GO_VERSION`         | `1.24`        | Image default      |

```java
Sandbox sandbox = Sandbox.builder()
    .image("opensandbox/code-interpreter:v1.0.2")
    .entrypoint("/opt/opensandbox/code-interpreter.sh")
    .env("JAVA_VERSION", "17")
    .env("GO_VERSION", "1.23")
    .build();
```

## Usage Examples

### 0. Run with `language` (default language context)

If you don't need to manage explicit session IDs, you can run code by specifying only `language`.
When `context.id` is omitted, **execd will create/reuse a default session for that language**, so
state can persist across runs:

```java
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.SupportedLanguage;

// Default Python context: state persists across runs
interpreter.codes().run("x = 42", SupportedLanguage.PYTHON);
Execution execution = interpreter.codes().run("result = x\nresult", SupportedLanguage.PYTHON);
System.out.println(execution.getResult().get(0).getText()); // 42
```

### 1. Java Code Execution

Execute Java code snippets dynamically.

```java
CodeContext javaContext = interpreter.codes().createContext(SupportedLanguage.JAVA);

RunCodeRequest request = RunCodeRequest.builder()
    .code(
        "System.out.println(\"Calculating sum...\");\n" +
        "int a = 10;\n" +
        "int b = 20;\n" +
        "int sum = a + b;\n" +
        "System.out.println(\"Sum: \" + sum);\n" +
        "sum" // Return value
    )
    .context(javaContext)
    .build();

Execution execution = interpreter.codes().run(request);

// Handle results
System.out.println("Execution ID: " + execution.getId());
execution.getLogs().getStdout().forEach(log -> System.out.println(log.getText()));
```

### 2. Python with State Persistence

Variables defined in one execution are available in subsequent executions within the same context.

```java
CodeContext pythonContext = interpreter.codes().createContext(SupportedLanguage.PYTHON);

// Step 1: Define variables
RunCodeRequest step1 = RunCodeRequest.builder()
    .code(
        "users = ['Alice', 'Bob', 'Charlie']\n" +
        "print(f'Initialized {len(users)} users')"
    )
    .context(pythonContext)
    .build();
interpreter.codes().run(step1);

// Step 2: Use variables from previous step
RunCodeRequest step2 = RunCodeRequest.builder()
    .code(
        "users.append('Dave')\n" +
        "print(f'Updated users: {users}')"
    )
    .context(pythonContext)
    .build();

Execution result = interpreter.codes().run(step2);
// Output: Updated users: ['Alice', 'Bob', 'Charlie', 'Dave']
```

### 3. Streaming Output Handling

Handle standard output, error output, and execution events in real-time.

```java
ExecutionHandlers handlers = ExecutionHandlers.builder()
    .onStdout(msg -> System.out.println("STDOUT: " + msg.getText()))
    .onStderr(msg -> System.err.println("STDERR: " + msg.getText()))
    .onResult(res -> System.out.println("Result: " + res.getText()))
    .onError(err -> System.err.println("Error: " + err.getValue()))
    .onExecutionComplete(complete ->
        System.out.println("Finished in " + complete.getExecutionTimeInMillis() + "ms")
    )
    .build();

RunCodeRequest request = RunCodeRequest.builder()
    .code("import time\nfor i in range(5):\n    print(i)\n    time.sleep(0.5)")
    .context(pythonContext)
    .handlers(handlers)
    .build();

interpreter.codes().run(request);
```

### 4. Multi-Language Context Isolation

Different languages run in isolated environments.

```java
CodeContext pyCtx = interpreter.codes().createContext(SupportedLanguage.PYTHON);
CodeContext goCtx = interpreter.codes().createContext(SupportedLanguage.GO);

// Python Context
interpreter.codes().run(
    RunCodeRequest.builder()
        .code("print('Running in Python')")
        .context(pyCtx)
        .build()
);

// Go Context
interpreter.codes().run(
    RunCodeRequest.builder()
        .code(
            "package main\n" +
            "func main() { println(\"Running in Go\") }"
        )
        .context(goCtx)
        .build()
);
```
