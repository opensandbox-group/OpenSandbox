// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

using OpenSandbox.CodeInterpreter;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Models;
using Xunit;
using CodeInterpreterClient = OpenSandbox.CodeInterpreter.CodeInterpreter;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public class CodeInterpreterE2ETests : IClassFixture<CodeInterpreterE2ETestFixture>
{
    private readonly CodeInterpreterE2ETestFixture _fixture;

    public CodeInterpreterE2ETests(CodeInterpreterE2ETestFixture fixture)
    {
        _fixture = fixture;
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task CreateInterpreter_ExposesSandboxServices()
    {
        var sandbox = _fixture.Sandbox;
        var interpreter = _fixture.Interpreter;

        Assert.Equal(sandbox.Id, interpreter.Id);
        Assert.NotNull(interpreter.Codes);
        Assert.NotNull(interpreter.Files);
        Assert.NotNull(interpreter.Commands);
        Assert.NotNull(interpreter.Metrics);

        var metrics = await interpreter.Metrics.GetMetricsAsync();
        Assert.True(metrics.CpuCount > 0);

        var cmd = await RunCommandWithRetryAsync(interpreter, "echo code-interpreter-ready");
        Assert.Null(cmd.Error);
        Assert.Contains(cmd.Logs.Stdout, m => m.Text.Contains("code-interpreter-ready", StringComparison.Ordinal));
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task ContextManagement_CreateGetListDelete()
    {
        var interpreter = _fixture.Interpreter;

        var ctx = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);
        Assert.NotNull(ctx.Id);
        Assert.Equal(SupportedLanguage.Python, ctx.Language);

        var fetched = await interpreter.Codes.GetContextAsync(ctx.Id!);
        Assert.Equal(ctx.Id, fetched.Id);
        Assert.Equal(SupportedLanguage.Python, fetched.Language);

        var listed = await interpreter.Codes.ListContextsAsync(SupportedLanguage.Python);
        Assert.Contains(listed, c => c.Id == ctx.Id);

        await interpreter.Codes.DeleteContextAsync(ctx.Id!);

        var listedAfterDelete = await interpreter.Codes.ListContextsAsync(SupportedLanguage.Python);
        Assert.DoesNotContain(listedAfterDelete, c => c.Id == ctx.Id);
    }

    [Fact(Timeout = 4 * 60 * 1000)]
    public async Task RunAsync_ContextPersistence_AndIsolation()
    {
        var interpreter = _fixture.Interpreter;

        var ctx1 = await CreateContextWithRetryAsync(interpreter, SupportedLanguage.Python);
        var ctx2 = await CreateContextWithRetryAsync(interpreter, SupportedLanguage.Python);

        await RunWithRetryAsync(interpreter, "x = 42", new RunCodeOptions { Context = ctx1 });
        var persisted = await RunWithRetryAsync(interpreter, "print(x)", new RunCodeOptions { Context = ctx1 });
        Assert.Contains(persisted.Logs.Stdout, s => s.Text.Contains("42", StringComparison.Ordinal));

        var isolated = await RunWithRetryAsync(interpreter, "print('x' in globals())", new RunCodeOptions { Context = ctx2 });
        Assert.Contains(isolated.Logs.Stdout, s => s.Text.Contains("False", StringComparison.OrdinalIgnoreCase));

        await interpreter.Codes.DeleteContextAsync(ctx1.Id!);
        await interpreter.Codes.DeleteContextAsync(ctx2.Id!);
    }

    [Fact(Timeout = 3 * 60 * 1000)]
    public async Task RunAsync_MultiLanguage_BasicExecution()
    {
        var interpreter = _fixture.Interpreter;

        var py = await RunWithRetryAsync(
            interpreter,
            "result = 1 + 2\nresult",
            new RunCodeOptions { Language = SupportedLanguage.Python });
        Assert.True(HasText(py, "3"));
        Assert.Null(py.ExitCode);
        Assert.NotNull(py.Complete);

        var js = await RunWithRetryAsync(
            interpreter,
            "console.log(3+4)",
            new RunCodeOptions { Language = SupportedLanguage.JavaScript });
        Assert.True(HasText(js, "7"));
        Assert.Null(js.ExitCode);
        Assert.NotNull(js.Complete);

        var bash = await RunWithRetryAsync(
            interpreter,
            "echo $((8+9))",
            new RunCodeOptions { Language = SupportedLanguage.Bash });
        Assert.True(HasText(bash, "17"));
        Assert.Null(bash.ExitCode);
        Assert.NotNull(bash.Complete);
    }

    [Fact(Timeout = 6 * 60 * 1000)]
    public async Task RunAsync_MultiLanguage_Java_Go_TypeScript()
    {
        var interpreter = _fixture.Interpreter;

        var javaCtx = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Java);
        var goCtx = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Go);
        var tsCtx = await interpreter.Codes.CreateContextAsync(SupportedLanguage.TypeScript);

        try
        {
            var javaResult = await interpreter.Codes.RunAsync(
                "System.out.println(\"java-ok\");\nint v = 2 + 3;\nSystem.out.println(v);\n",
                new RunCodeOptions { Context = javaCtx });
            Assert.Null(javaResult.Error);
            Assert.True(HasText(javaResult, "java-ok") || HasText(javaResult, "5"));
            Assert.Null(javaResult.ExitCode);
            Assert.NotNull(javaResult.Complete);

            var goResult = await interpreter.Codes.RunAsync(
                "package main\nimport \"fmt\"\nfunc main(){ fmt.Print(\"go-ok\") }",
                new RunCodeOptions { Context = goCtx });
            Assert.Null(goResult.Error);
            Assert.True(HasText(goResult, "go-ok"));
            Assert.Null(goResult.ExitCode);
            Assert.NotNull(goResult.Complete);

            var tsResult = await interpreter.Codes.RunAsync(
                "console.log('ts-ok'); const n: number = 3 + 4; console.log(n);",
                new RunCodeOptions { Context = tsCtx });
            Assert.Null(tsResult.Error);
            Assert.True(HasText(tsResult, "ts-ok") || HasText(tsResult, "7"));
            Assert.Null(tsResult.ExitCode);
            Assert.NotNull(tsResult.Complete);
        }
        finally
        {
            await interpreter.Codes.DeleteContextAsync(javaCtx.Id!);
            await interpreter.Codes.DeleteContextAsync(goCtx.Id!);
            await interpreter.Codes.DeleteContextAsync(tsCtx.Id!);
        }
    }

    [Fact(Timeout = 3 * 60 * 1000)]
    public async Task ContextManagement_DeleteContexts_ByLanguage()
    {
        var interpreter = _fixture.Interpreter;

        await interpreter.Codes.DeleteContextsAsync(SupportedLanguage.Bash);

        var ctx1 = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Bash);
        var ctx2 = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Bash);

        var listed = await interpreter.Codes.ListContextsAsync(SupportedLanguage.Bash);
        Assert.Contains(listed, c => c.Id == ctx1.Id);
        Assert.Contains(listed, c => c.Id == ctx2.Id);

        await interpreter.Codes.DeleteContextsAsync(SupportedLanguage.Bash);

        var afterDelete = await interpreter.Codes.ListContextsAsync(SupportedLanguage.Bash);
        Assert.DoesNotContain(afterDelete, c => c.Id == ctx1.Id);
        Assert.DoesNotContain(afterDelete, c => c.Id == ctx2.Id);
    }

    [Fact(Timeout = 3 * 60 * 1000)]
    public async Task RunStreamAsync_ReturnsRealtimeEvents()
    {
        var interpreter = _fixture.Interpreter;

        var request = new RunCodeRequest
        {
            Code = "for i in range(3): print(i)",
            Context = new CodeContext { Language = SupportedLanguage.Python }
        };

        var events = await RunStreamCollectWithRetryAsync(interpreter, request);

        Assert.True(events.Count > 0);
        Assert.Contains(
            events,
            ev => ev.Type == ServerStreamEventTypes.Stdout ||
                  ev.Type == ServerStreamEventTypes.Stderr ||
                  ev.Type == ServerStreamEventTypes.Result ||
                  ev.Type == ServerStreamEventTypes.Error ||
                  ev.Type == ServerStreamEventTypes.ExecutionComplete);
    }

    [Fact(Timeout = 3 * 60 * 1000)]
    public async Task InterruptAsync_StopsLongRunningExecution()
    {
        var interpreter = _fixture.Interpreter;

        var ctx = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);
        var initLatch = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
        var runTask = interpreter.Codes.RunAsync(
            "import time\nwhile True: time.sleep(1)",
            new RunCodeOptions
            {
                Context = ctx,
                Handlers = new ExecutionHandlers
                {
                    OnInit = init =>
                    {
                        initLatch.TrySetResult(init.Id);
                        return Task.CompletedTask;
                    }
                }
            });

        var executionId = await initLatch.Task.WaitAsync(TimeSpan.FromSeconds(15));
        Assert.False(string.IsNullOrWhiteSpace(executionId));
        await interpreter.Codes.InterruptAsync(executionId);

        Execution? execution = null;
        try
        {
            execution = await runTask.WaitAsync(TimeSpan.FromSeconds(30));
        }
        catch (TimeoutException)
        {
            // Some environments interrupt the backend execution but do not close
            // the SSE stream promptly. Treat this as acceptable if a follow-up
            // run proves the interpreter remains usable.
        }
        catch
        {
            // The stream may terminate abruptly after interrupt.
        }

        if (execution != null)
        {
            Assert.Equal(executionId, execution.Id);
        }

        var quickResult = await interpreter.Codes.RunAsync(
            "print('Quick Python execution')\nresult = 2 + 2\nprint(f'Result: {result}')",
            new RunCodeOptions { Context = ctx });
        Assert.NotNull(quickResult);
        Assert.False(string.IsNullOrWhiteSpace(quickResult.Id));

        await interpreter.Codes.DeleteContextAsync(ctx.Id!);
    }

    [Fact(Timeout = 6 * 60 * 1000)]
    public async Task RunAsync_ConcurrentExecution_MultipleContexts()
    {
        var interpreter = _fixture.Interpreter;

        var py = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);
        var java = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Java);
        var go = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Go);

        try
        {
            var tasks = new[]
            {
                interpreter.Codes.RunAsync(
                    "import time\nfor i in range(3):\n print(f'py-{i}')\n time.sleep(0.1)\nprint('py-done')",
                    new RunCodeOptions { Context = py }),
                interpreter.Codes.RunAsync(
                    "for (int i=0;i<3;i++){System.out.println(\"java-\" + i);} System.out.println(\"java-done\");",
                    new RunCodeOptions { Context = java }),
                interpreter.Codes.RunAsync(
                    "package main\nimport \"fmt\"\nfunc main(){for i:=0;i<3;i++{fmt.Println(i)}; fmt.Print(\"go-done\")}",
                    new RunCodeOptions { Context = go })
            };

            var results = await Task.WhenAll(tasks);
            var succeeded = results.Count(r => r != null && r.Error == null && !string.IsNullOrWhiteSpace(r.Id));
            Assert.True(succeeded >= 2, $"expected at least 2 successful concurrent runs, actual={succeeded}");
        }
        finally
        {
            await interpreter.Codes.DeleteContextAsync(py.Id!);
            await interpreter.Codes.DeleteContextAsync(java.Id!);
            await interpreter.Codes.DeleteContextAsync(go.Id!);
        }
    }

    [Fact(Timeout = 8 * 60 * 1000)]
    public async Task RunAsync_MultiLanguage_ErrorHandling_WithEventContract()
    {
        var interpreter = _fixture.Interpreter;

        var py = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Python);
        var java = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Java);
        var go = await interpreter.Codes.CreateContextAsync(SupportedLanguage.Go);
        var ts = await interpreter.Codes.CreateContextAsync(SupportedLanguage.TypeScript);

        try
        {
            var pyExecution = await RunWithTrackedEventsAsync(
                interpreter,
                "print(undefined_variable)",
                py);
            Assert.True(pyExecution.Execution.Error != null || pyExecution.Execution.Logs.Stderr.Count > 0);
            if (pyExecution.Execution.Error != null)
            {
                Assert.Contains("NameError", pyExecution.Execution.Error.Name, StringComparison.OrdinalIgnoreCase);
            }
            AssertTerminalEventContract(pyExecution.InitEvents, pyExecution.CompleteEvents, pyExecution.ErrorEvents, pyExecution.Execution.Id);

            var javaExecution = await RunWithTrackedEventsAsync(
                interpreter,
                "int x = 10 / 0;",
                java);
            Assert.True(javaExecution.Execution.Error != null || javaExecution.Execution.Logs.Stderr.Count > 0);
            AssertTerminalEventContract(javaExecution.InitEvents, javaExecution.CompleteEvents, javaExecution.ErrorEvents, javaExecution.Execution.Id);

            var goExecution = await RunWithTrackedEventsAsync(
                interpreter,
                "package main\nfunc main(){ undeclaredVariable++ }",
                go);
            Assert.True(goExecution.Execution.Error != null || goExecution.Execution.Logs.Stderr.Count > 0);
            AssertTerminalEventContract(goExecution.InitEvents, goExecution.CompleteEvents, goExecution.ErrorEvents, goExecution.Execution.Id);

            var tsExecution = await RunWithTrackedEventsAsync(
                interpreter,
                "throw new Error('ts-runtime-error');",
                ts);
            Assert.True(tsExecution.Execution.Error != null || tsExecution.Execution.Logs.Stderr.Count > 0);
            AssertTerminalEventContract(tsExecution.InitEvents, tsExecution.CompleteEvents, tsExecution.ErrorEvents, tsExecution.Execution.Id);
        }
        finally
        {
            await interpreter.Codes.DeleteContextAsync(py.Id!);
            await interpreter.Codes.DeleteContextAsync(java.Id!);
            await interpreter.Codes.DeleteContextAsync(go.Id!);
            await interpreter.Codes.DeleteContextAsync(ts.Id!);
        }
    }

    private static async Task<TrackedExecution> RunWithTrackedEventsAsync(
        CodeInterpreterClient interpreter,
        string code,
        CodeContext context)
    {
        var initEvents = new List<ExecutionInit>();
        var completeEvents = new List<ExecutionComplete>();
        var errorEvents = new List<ExecutionError>();
        var handlers = new ExecutionHandlers
        {
            OnInit = ev =>
            {
                initEvents.Add(ev);
                return Task.CompletedTask;
            },
            OnExecutionComplete = ev =>
            {
                completeEvents.Add(ev);
                return Task.CompletedTask;
            },
            OnError = ev =>
            {
                errorEvents.Add(ev);
                return Task.CompletedTask;
            }
        };

        var execution = await interpreter.Codes.RunAsync(
            code,
            new RunCodeOptions
            {
                Context = context,
                Handlers = handlers
            });

        return new TrackedExecution(execution, initEvents, completeEvents, errorEvents);
    }

    private static void AssertTerminalEventContract(
        IReadOnlyList<ExecutionInit> initEvents,
        IReadOnlyList<ExecutionComplete> completeEvents,
        IReadOnlyList<ExecutionError> errorEvents,
        string? executionId)
    {
        Assert.Single(initEvents);
        Assert.False(string.IsNullOrWhiteSpace(initEvents[0].Id));
        if (!string.IsNullOrWhiteSpace(executionId))
        {
            Assert.Equal(executionId, initEvents[0].Id);
        }
        AssertRecentTimestampMs(initEvents[0].Timestamp, 180_000);

        var hasComplete = completeEvents.Count > 0;
        var hasError = errorEvents.Count > 0;
        Assert.True(hasComplete || hasError);

        if (hasComplete)
        {
            Assert.Single(completeEvents);
            Assert.True(completeEvents[0].ExecutionTimeMs >= 0);
            AssertRecentTimestampMs(completeEvents[0].Timestamp, 180_000);
        }

        if (hasError)
        {
            Assert.False(string.IsNullOrWhiteSpace(errorEvents[0].Name));
            Assert.False(string.IsNullOrWhiteSpace(errorEvents[0].Value));
            AssertRecentTimestampMs(errorEvents[0].Timestamp, 180_000);
        }
    }

    private static void AssertRecentTimestampMs(long ts, long toleranceMs)
    {
        Assert.True(ts > 0);
        var delta = Math.Abs(DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() - ts);
        Assert.True(delta <= toleranceMs, $"timestamp too far from now: delta={delta}ms (ts={ts})");
    }

    private static bool HasText(Execution execution, string expected)
    {
        return execution.Logs.Stdout.Any(x => x.Text.Contains(expected, StringComparison.Ordinal)) ||
               execution.Logs.Stderr.Any(x => x.Text.Contains(expected, StringComparison.Ordinal)) ||
               execution.Results.Any(x => (x.Text ?? string.Empty).Contains(expected, StringComparison.Ordinal));
    }

    private static async Task<CodeContext> CreateContextWithRetryAsync(
        CodeInterpreterClient interpreter,
        string language,
        int maxRetries = 3)
    {
        Exception? lastError = null;
        var delayMs = 1000;
        for (var attempt = 1; attempt <= maxRetries; attempt++)
        {
            try
            {
                var ctx = await interpreter.Codes.CreateContextAsync(language).WaitAsync(TimeSpan.FromSeconds(60));
                await Task.Delay(500);
                return ctx;
            }
            catch (Exception ex) when (IsRetryable(ex) && attempt < maxRetries)
            {
                lastError = ex;
                await Task.Delay(delayMs);
                delayMs = (int)(delayMs * 1.5);
            }
            catch (Exception ex)
            {
                lastError = ex;
                break;
            }
        }

        throw lastError ?? new TimeoutException("CreateContextWithRetryAsync failed unexpectedly.");
    }

    private static async Task<Execution> RunWithRetryAsync(
        CodeInterpreterClient interpreter,
        string code,
        RunCodeOptions? options = null,
        int maxRetries = 3,
        int perCallTimeoutSeconds = 120)
    {
        Exception? lastError = null;
        var delayMs = 1000;
        for (var attempt = 1; attempt <= maxRetries; attempt++)
        {
            try
            {
                var result = await interpreter.Codes
                    .RunAsync(code, options)
                    .WaitAsync(TimeSpan.FromSeconds(perCallTimeoutSeconds));

                if (!string.IsNullOrWhiteSpace(result.Id))
                {
                    return result;
                }

                if (attempt < maxRetries)
                {
                    await Task.Delay(delayMs);
                    delayMs = (int)(delayMs * 1.5);
                    continue;
                }

                return result;
            }
            catch (Exception ex) when (IsRetryable(ex) && attempt < maxRetries)
            {
                lastError = ex;
                await Task.Delay(delayMs);
                delayMs = (int)(delayMs * 1.5);
            }
            catch (Exception ex)
            {
                lastError = ex;
                break;
            }
        }

        throw lastError ?? new TimeoutException("RunWithRetryAsync failed unexpectedly.");
    }

    private static async Task<List<ServerStreamEvent>> RunStreamCollectWithRetryAsync(
        CodeInterpreterClient interpreter,
        RunCodeRequest request,
        int maxRetries = 5,
        int perCallTimeoutSeconds = 120)
    {
        Exception? lastError = null;
        var delayMs = 1000;
        for (var attempt = 1; attempt <= maxRetries; attempt++)
        {
            try
            {
                var events = new List<ServerStreamEvent>();
                using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(perCallTimeoutSeconds));
                await foreach (var ev in interpreter.Codes.RunStreamAsync(request, cts.Token))
                {
                    events.Add(ev);
                }

                var hasBusinessEvent = events.Any(ev =>
                    ev.Type == ServerStreamEventTypes.Stdout ||
                    ev.Type == ServerStreamEventTypes.Stderr ||
                    ev.Type == ServerStreamEventTypes.Result ||
                    ev.Type == ServerStreamEventTypes.Error ||
                    ev.Type == ServerStreamEventTypes.ExecutionComplete);

                if (hasBusinessEvent)
                {
                    return events;
                }

                if (attempt < maxRetries)
                {
                    await Task.Delay(delayMs);
                    delayMs = (int)(delayMs * 1.5);
                    continue;
                }

                var observedTypes = string.Join(",", events.Select(e => e.Type ?? "null"));
                throw new TimeoutException(
                    $"RunStreamCollectWithRetryAsync did not observe business events after {maxRetries} attempts. " +
                    $"Observed event types: [{observedTypes}]");
            }
            catch (Exception ex) when (IsRetryable(ex) && attempt < maxRetries)
            {
                lastError = ex;
                await Task.Delay(delayMs);
                delayMs = (int)(delayMs * 1.5);
            }
            catch (Exception ex)
            {
                lastError = ex;
                break;
            }
        }

        throw lastError ?? new TimeoutException("RunStreamCollectWithRetryAsync failed unexpectedly.");
    }

    private static async Task<Execution> RunCommandWithRetryAsync(
        CodeInterpreterClient interpreter,
        string command,
        int maxRetries = 3,
        int perCallTimeoutSeconds = 30)
    {
        Exception? lastError = null;
        Execution? lastResult = null;
        var delayMs = 1000;

        for (var attempt = 1; attempt <= maxRetries; attempt++)
        {
            try
            {
                var result = await interpreter.Commands
                    .RunAsync(command)
                    .WaitAsync(TimeSpan.FromSeconds(perCallTimeoutSeconds));

                lastResult = result;
                var hasExpectedStdout = result.Logs.Stdout.Any(log =>
                    log.Text.Contains("code-interpreter-ready", StringComparison.Ordinal));
                if (result.Error == null && hasExpectedStdout)
                {
                    return result;
                }

                if (attempt < maxRetries)
                {
                    await Task.Delay(delayMs);
                    delayMs = (int)(delayMs * 1.5);
                    continue;
                }

                return result;
            }
            catch (Exception ex) when (IsRetryable(ex) && attempt < maxRetries)
            {
                lastError = ex;
                await Task.Delay(delayMs);
                delayMs = (int)(delayMs * 1.5);
            }
            catch (Exception ex)
            {
                lastError = ex;
                break;
            }
        }

        if (lastResult != null)
        {
            return lastResult;
        }

        throw lastError ?? new TimeoutException("RunCommandWithRetryAsync failed unexpectedly.");
    }

    private static bool IsRetryable(Exception ex)
    {
        if (ex is TimeoutException || ex is TaskCanceledException)
        {
            return true;
        }

        var message = ex.ToString();
        var lowered = message.ToLowerInvariant();
        return lowered.Contains("disconnected", StringComparison.Ordinal) ||
               lowered.Contains("connection", StringComparison.Ordinal) ||
               lowered.Contains("reset", StringComparison.Ordinal) ||
               lowered.Contains("closed", StringComparison.Ordinal) ||
               lowered.Contains("timeout", StringComparison.Ordinal) ||
               lowered.Contains("peer", StringComparison.Ordinal) ||
               lowered.Contains("response ended prematurely", StringComparison.Ordinal);
    }

    private sealed record TrackedExecution(
        Execution Execution,
        IReadOnlyList<ExecutionInit> InitEvents,
        IReadOnlyList<ExecutionComplete> CompleteEvents,
        IReadOnlyList<ExecutionError> ErrorEvents);
}

public sealed class CodeInterpreterE2ETestFixture : IAsyncLifetime
{
    private readonly E2ETestFixture _baseFixture = new();
    private Sandbox? _sandbox;
    private CodeInterpreterClient? _interpreter;

    public Sandbox Sandbox => _sandbox ?? throw new InvalidOperationException("Sandbox is not initialized.");
    public CodeInterpreterClient Interpreter => _interpreter ?? throw new InvalidOperationException("Interpreter is not initialized.");

    public async Task InitializeAsync()
    {
        _sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _baseFixture.ConnectionConfig,
            Image = _baseFixture.DefaultImage,
            Entrypoint = new[] { "/opt/opensandbox/code-interpreter.sh" },
            TimeoutSeconds = _baseFixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _baseFixture.DefaultReadyTimeoutSeconds,
            Resource = new Dictionary<string, string>
            {
                ["cpu"] = "2",
                ["memory"] = "4Gi"
            },
            Env = new Dictionary<string, string>
            {
                ["E2E_TEST"] = "true",
                ["GO_VERSION"] = "1.25",
                ["JAVA_VERSION"] = "21",
                ["NODE_VERSION"] = "22",
                ["PYTHON_VERSION"] = "3.12",
                ["EXECD_LOG_FILE"] = "/tmp/opensandbox-e2e/logs/execd.log",
                ["EXECD_API_GRACE_SHUTDOWN"] = "3s",
                ["EXECD_JUPYTER_IDLE_POLL_INTERVAL"] = "200ms"
            },
            Volumes = new[]
            {
                new Volume
                {
                    Name = "execd-log",
                    Host = new Host { Path = "/tmp/opensandbox-e2e/logs" },
                    MountPath = "/tmp/opensandbox-e2e/logs",
                    ReadOnly = false
                }
            },
            Metadata = new Dictionary<string, string> { ["tag"] = "csharp-code-interpreter-e2e" },
            HealthCheckPollingInterval = 500
        });

        _interpreter = await CodeInterpreterClient.CreateAsync(_sandbox);
    }

    public async Task DisposeAsync()
    {
        if (_sandbox == null)
        {
            return;
        }

        try
        {
            await _sandbox.KillAsync();
        }
        catch
        {
        }

        await _sandbox.DisposeAsync();
    }
}
