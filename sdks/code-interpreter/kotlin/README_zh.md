# Alibaba Code Interpreter SDK for Kotlin

中文 | [English](README.md)

一个用于在安全、隔离的沙箱环境中执行代码的 Kotlin SDK。该 SDK 提供了高级 API，支持安全地运行 Python、Java、Go、TypeScript 等语言，并具备代码执行上下文（Context）能力。

## 前置要求

本 SDK 需要配合包含 Code Interpreter 运行时环境的特定 Docker 镜像使用。请务必使用 `opensandbox/code-interpreter` 镜像（或其衍生镜像），其中预装了 Python、Java、Go、Node.js 等语言的运行环境。

## 安装指南

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

## 快速开始

以下示例展示了如何初始化客户端，指定 Python 版本并执行一段简单的脚本。

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
        // 1. 配置连接信息
        ConnectionConfig config = ConnectionConfig.builder()
            .domain("api.opensandbox.io")
            .apiKey("your-api-key")
            .build();

        // 2. 创建 Sandbox 实例
        // 注意: 必须使用 code-interpreter 专用镜像
        // 使用 try-with-resources 确保资源正确关闭
        try (Sandbox sandbox = Sandbox.builder()
                .connectionConfig(config)
                .image("sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2")
                .entrypoint("/opt/opensandbox/code-interpreter.sh")
                .env("PYTHON_VERSION", "3.11") // 指定语言版本
                .build()) {

            // 3. 创建 CodeInterpreter 包装器
            CodeInterpreter interpreter = CodeInterpreter.builder()
                .fromSandbox(sandbox)
                .build();

            // 4. 创建执行上下文 (Python)
            CodeContext context = interpreter.codes().createContext(SupportedLanguage.PYTHON);

            // 5. 运行代码
            Execution result = interpreter.codes().run(
                RunCodeRequest.builder()
                    .code("import sys; print(f'Running on Python {sys.version}')")
                    .context(context)
                    .build()
            );

            // 6. 打印输出
            if (!result.getLogs().getStdout().isEmpty()) {
                System.out.println(result.getLogs().getStdout().get(0).getText());
            }

            // 7. 清理资源
            // 注意: kill() 会立即终止远程沙箱实例；try-with-resources 会自动调用 close() 清理本地资源
            sandbox.kill();
        } catch (SandboxException e) {
            // 处理 Sandbox 特定异常
            System.err.println("沙箱错误: [" + e.getError().getCode() + "] " + e.getError().getMessage());
        } catch (Exception e) {
            e.printStackTrace();
        }
    }
}
```

## 运行时配置

### Docker 镜像

Code Interpreter SDK 依赖于特定的运行环境。请确保你的沙箱服务提供商支持 `opensandbox/code-interpreter` 镜像。

关于支持的语言和具体版本的详细信息，请参考 [环境文档](../../../sandboxes/code-interpreter/README_zh.md)。

### 排错：`pip: command not found`

如果你在 Kotlin / Java 客户端里执行了 `pip install pandas` 这类命令，请先确认
当前沙箱使用的是 `opensandbox/code-interpreter` 镜像（或其衍生镜像），并且入口点
是 Code Interpreter 的启动脚本：

```java
Sandbox sandbox = Sandbox.builder()
    .image("opensandbox/code-interpreter:v1.0.2")
    .entrypoint("/opt/opensandbox/code-interpreter.sh")
    .env("PYTHON_VERSION", "3.11")
    .build();
```

普通 Sandbox SDK 也可以连接任意通用沙箱镜像，但这些镜像并不保证预装 Python 或
`pip`。在这种情况下，执行 `pip install ...` 会报 `pip: command not found`。
如果需要 Python 运行时和包管理能力，请切换到 Code Interpreter 镜像；否则请使用
与你当前镜像匹配的包管理方式。

### 语言版本选择

你可以在创建 `Sandbox` 时通过环境变量指定所需的编程语言版本。

| 语言    | 环境变量         | 示例值 | 默认值 (若不设置) |
| ------- | ---------------- | ------ | ----------------- |
| Python  | `PYTHON_VERSION` | `3.11` | 镜像默认值        |
| Java    | `JAVA_VERSION`   | `17`   | 镜像默认值        |
| Node.js | `NODE_VERSION`   | `20`   | 镜像默认值        |
| Go      | `GO_VERSION`     | `1.24` | 镜像默认值        |

```java
Sandbox sandbox = Sandbox.builder()
    .image("opensandbox/code-interpreter:v1.0.2")
    .entrypoint("/opt/opensandbox/code-interpreter.sh")
    .env("JAVA_VERSION", "17")
    .env("GO_VERSION", "1.23")
    .build();
```

## 核心功能示例

### 0. 直接传 `language`（使用该语言默认上下文）

如果你不需要显式管理 session id，可以只传 `language` 来执行代码。
当 `context.id` 省略时，**execd 会为该语言创建/复用默认 session**，因此状态可以跨次执行保持：

```java
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.SupportedLanguage;

// Python 默认上下文：状态会在多次 run 之间保持
interpreter.codes().run("x = 42", SupportedLanguage.PYTHON);
Execution execution = interpreter.codes().run("result = x\nresult", SupportedLanguage.PYTHON);
System.out.println(execution.getResult().get(0).getText()); // 42
```

### 1. Java 代码执行

动态执行 Java 代码片段。

```java
CodeContext javaContext = interpreter.codes().createContext(SupportedLanguage.JAVA);

RunCodeRequest request = RunCodeRequest.builder()
    .code(
        "System.out.println(\"Calculating sum...\");\n" +
        "int a = 10;\n" +
        "int b = 20;\n" +
        "int sum = a + b;\n" +
        "System.out.println(\"Sum: \" + sum);\n" +
        "sum" // 返回值
    )
    .context(javaContext)
    .build();

Execution execution = interpreter.codes().run(request);

// 处理结果
System.out.println("Execution ID: " + execution.getId());
execution.getLogs().getStdout().forEach(log -> System.out.println(log.getText()));
```

### 2. Python 持久化状态

在同一个上下文中，变量状态可以跨次执行保持。

```java
CodeContext pythonContext = interpreter.codes().createContext(SupportedLanguage.PYTHON);

// 步骤 1: 定义变量
RunCodeRequest step1 = RunCodeRequest.builder()
    .code(
        "users = ['Alice', 'Bob', 'Charlie']\n" +
        "print(f'Initialized {len(users)} users')"
    )
    .context(pythonContext)
    .build();
interpreter.codes().run(step1);

// 步骤 2: 使用上一步的变量
RunCodeRequest step2 = RunCodeRequest.builder()
    .code(
        "users.append('Dave')\n" +
        "print(f'Updated users: {users}')"
    )
    .context(pythonContext)
    .build();

Execution result = interpreter.codes().run(step2);
// 输出: Updated users: ['Alice', 'Bob', 'Charlie', 'Dave']
```

### 3. 流式输出处理

实时处理标准输出、错误输出和执行事件。

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

### 4. 多语言上下文隔离

不同语言在隔离的环境中运行。

```java
CodeContext pyCtx = interpreter.codes().createContext(SupportedLanguage.PYTHON);
CodeContext goCtx = interpreter.codes().createContext(SupportedLanguage.GO);

// Python 上下文
interpreter.codes().run(
    RunCodeRequest.builder()
        .code("print('Running in Python')")
        .context(pyCtx)
        .build()
);

// Go 上下文
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
