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

using System.Net;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using Xunit;

namespace OpenSandbox.Tests;

public class CommandsAdapterTests
{
    [Fact]
    public async Task GetCommandStatusAsync_ShouldParseStatusResponse()
    {
        var httpHandler = new StubHttpMessageHandler((request, _) =>
        {
            var body = "{\"id\":\"cmd-1\",\"content\":\"sleep 1\",\"running\":true,\"exit_code\":null}";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(body, Encoding.UTF8, "application/json")
            });
        });
        var adapter = CreateAdapter(httpHandler);

        var status = await adapter.GetCommandStatusAsync("cmd-1");

        status.Id.Should().Be("cmd-1");
        status.Content.Should().Be("sleep 1");
        status.Running.Should().BeTrue();
        status.ExitCode.Should().BeNull();
        httpHandler.RequestUris.Should().Contain(uri => uri.EndsWith("/command/status/cmd-1"));
    }

    [Fact]
    public async Task GetBackgroundCommandLogsAsync_ShouldParseCursorHeader()
    {
        var httpHandler = new StubHttpMessageHandler((request, _) =>
        {
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("line1\nline2\n", Encoding.UTF8, "text/plain")
            };
            response.Headers.Add("EXECD-COMMANDS-TAIL-CURSOR", "42");
            return Task.FromResult(response);
        });
        var adapter = CreateAdapter(httpHandler);

        var logs = await adapter.GetBackgroundCommandLogsAsync("cmd-2", cursor: 10);

        logs.Content.Should().Contain("line1");
        logs.Cursor.Should().Be(42);
        httpHandler.RequestUris.Should().Contain(uri => uri.Contains("/command/cmd-2/logs?cursor=10"));
    }

    [Fact]
    public async Task GetBackgroundCommandLogsAsync_ShouldReturnNullCursorWhenHeaderMissing()
    {
        var httpHandler = new StubHttpMessageHandler((request, _) =>
        {
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("only-content", Encoding.UTF8, "text/plain")
            });
        });
        var adapter = CreateAdapter(httpHandler);

        var logs = await adapter.GetBackgroundCommandLogsAsync("cmd-3");

        logs.Content.Should().Be("only-content");
        logs.Cursor.Should().BeNull();
    }

    [Fact]
    public async Task RunStreamAsync_ShouldSendTimeoutInMilliseconds()
    {
        var handler = new StubHttpMessageHandler(async (request, _) =>
        {
            request.Content.Should().NotBeNull();
            var body = await request.Content!.ReadAsStringAsync().ConfigureAwait(false);
            using var doc = JsonDocument.Parse(body);
            doc.RootElement.GetProperty("timeout").GetInt64().Should().Be(2000);

            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("data: {\"type\":\"init\",\"text\":\"cmd-1\"}\n\n", Encoding.UTF8, "text/event-stream")
            };
        });
        var adapter = CreateAdapter(handler);

        var options = new RunCommandOptions
        {
            TimeoutSeconds = 2
        };

        await foreach (var _ in adapter.RunStreamAsync("sleep 1", options))
        {
            // Drain events.
        }
    }

    [Fact]
    public async Task RunStreamAsync_ShouldSendUidGidAndEnvs()
    {
        var handler = new StubHttpMessageHandler(async (request, _) =>
        {
            request.Content.Should().NotBeNull();
            var body = await request.Content!.ReadAsStringAsync().ConfigureAwait(false);
            using var doc = JsonDocument.Parse(body);
            doc.RootElement.GetProperty("uid").GetInt32().Should().Be(1000);
            doc.RootElement.GetProperty("gid").GetInt32().Should().Be(1000);
            var envs = doc.RootElement.GetProperty("envs");
            envs.GetProperty("APP_ENV").GetString().Should().Be("test");
            envs.GetProperty("LOG_LEVEL").GetString().Should().Be("debug");

            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("data: {\"type\":\"init\",\"text\":\"cmd-1\"}\n\n", Encoding.UTF8, "text/event-stream")
            };
        });
        var adapter = CreateAdapter(handler);

        var options = new RunCommandOptions
        {
            Uid = 1000,
            Gid = 1000,
            Envs = new Dictionary<string, string>
            {
                ["APP_ENV"] = "test",
                ["LOG_LEVEL"] = "debug"
            }
        };

        await foreach (var _ in adapter.RunStreamAsync("id", options))
        {
            // Drain events.
        }
    }

    [Fact]
    public async Task RunStreamAsync_ShouldRejectGidWithoutUid()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            throw new InvalidOperationException("HTTP should not be called when options are invalid.");
        });
        var adapter = CreateAdapter(handler);

        var options = new RunCommandOptions
        {
            Gid = 1000
        };

        var act = async () =>
        {
            await foreach (var _ in adapter.RunStreamAsync("id", options))
            {
                // Drain events.
            }
        };

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("*uid is required when gid is provided*");
    }

    [Fact]
    public async Task RunAsync_ShouldPopulateCompleteAndExitCodeForSuccessfulForegroundCommands()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            const string sse = """
data: {"type":"init","text":"cmd-1","timestamp":1}

data: {"type":"stdout","text":"hi","timestamp":2}

data: {"type":"execution_complete","timestamp":3,"execution_time":4}

""";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(sse, Encoding.UTF8, "text/event-stream")
            });
        });
        var adapter = CreateAdapter(handler);

        var execution = await adapter.RunAsync("echo hi");

        execution.Id.Should().Be("cmd-1");
        execution.Logs.Stdout.Should().ContainSingle().Which.Text.Should().Be("hi");
        execution.Complete.Should().NotBeNull();
        execution.Complete!.ExecutionTimeMs.Should().Be(4);
        execution.ExitCode.Should().Be(0);
    }

    [Fact]
    public async Task RunAsync_ShouldInferNonZeroExitCodeFromFinalErrorState()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            const string sse = """
data: {"type":"init","text":"cmd-2","timestamp":1}

data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}

""";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(sse, Encoding.UTF8, "text/event-stream")
            });
        });
        var adapter = CreateAdapter(handler);

        var execution = await adapter.RunAsync("exit 7");

        execution.Id.Should().Be("cmd-2");
        execution.Error.Should().NotBeNull();
        execution.Error!.Value.Should().Be("7");
        execution.Complete.Should().BeNull();
        execution.ExitCode.Should().Be(7);
    }

    [Fact]
    public async Task RunAsync_ShouldKeepExitCodeNullWhenErrorValueIsEmpty()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            const string sse = """
data: {"type":"init","text":"cmd-3","timestamp":1}

data: {"type":"execution_complete","timestamp":2,"execution_time":4}

data: {"type":"error","error":{"ename":"CommandExecError","evalue":"","traceback":["failed"]},"timestamp":3}

""";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(sse, Encoding.UTF8, "text/event-stream")
            });
        });
        var adapter = CreateAdapter(handler);

        var execution = await adapter.RunAsync("bad command");

        execution.Id.Should().Be("cmd-3");
        execution.Error.Should().NotBeNull();
        execution.Error!.Value.Should().BeEmpty();
        execution.Complete.Should().NotBeNull();
        execution.ExitCode.Should().BeNull();
    }

    // --- Bash session API integration tests ---

    [Fact]
    public async Task CreateSessionAsync_ShouldReturnSessionId_WhenWorkingDirectoryProvided()
    {
        var handler = new StubHttpMessageHandler(async (request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Post);
            request.RequestUri!.ToString().Should().Contain("/session");
            request.Content.Should().NotBeNull();
            var body = await request.Content!.ReadAsStringAsync().ConfigureAwait(false);
            using var doc = JsonDocument.Parse(body);
            doc.RootElement.GetProperty("cwd").GetString().Should().Be("/tmp");

            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("{\"session_id\":\"sess-abc123\"}", Encoding.UTF8, "application/json")
            };
        });
        var adapter = CreateAdapter(handler);

        var sessionId = await adapter.CreateSessionAsync(new CreateSessionOptions { WorkingDirectory = "/tmp" });

        sessionId.Should().Be("sess-abc123");
        handler.RequestUris.Should().Contain(uri => uri.EndsWith("/session"));
    }

    [Fact]
    public async Task CreateSessionAsync_ShouldReturnSessionId_WhenNoOptions()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Post);
            request.RequestUri!.ToString().Should().Contain("/session");

            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("{\"session_id\":\"sess-default\"}", Encoding.UTF8, "application/json")
            });
        });
        var adapter = CreateAdapter(handler);

        var sessionId = await adapter.CreateSessionAsync();

        sessionId.Should().Be("sess-default");
    }

    [Fact]
    public async Task CreateSessionAsync_ShouldThrow_WhenResponseHasEmptySessionId()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent("{\"session_id\":\"\"}", Encoding.UTF8, "application/json")
            });
        });
        var adapter = CreateAdapter(handler);

        var act = () => adapter.CreateSessionAsync();

        await act.Should().ThrowAsync<SandboxApiException>()
            .WithMessage("*empty session_id*");
    }

    [Fact]
    public async Task RunInSessionAsync_ShouldSendCommandAndOptions()
    {
        var handler = new StubHttpMessageHandler(async (request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Post);
            request.RequestUri!.ToString().Should().Contain("/session/sess-1/run");
            request.Content.Should().NotBeNull();
            var body = await request.Content!.ReadAsStringAsync().ConfigureAwait(false);
            using var doc = JsonDocument.Parse(body);
            doc.RootElement.GetProperty("command").GetString().Should().Be("pwd");
            doc.RootElement.GetProperty("cwd").GetString().Should().Be("/var");
            doc.RootElement.GetProperty("timeout").GetInt64().Should().Be(5000);

            var sse = "data: {\"type\":\"stdout\",\"text\":\"/var\"}\ndata: {\"type\":\"execution_complete\"}\n";
            return new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(sse, Encoding.UTF8, "text/event-stream")
            };
        });
        var adapter = CreateAdapter(handler);

        var run = await adapter.RunInSessionAsync(
            "sess-1",
            "pwd",
            new RunInSessionOptions { WorkingDirectory = "/var", TimeoutSeconds = 5 });

        run.Should().NotBeNull();
        run.Logs.Stdout.Should().ContainSingle(m => m.Text == "/var");
        run.ExitCode.Should().Be(0);
        handler.RequestUris.Should().Contain(uri => uri.Contains("/session/sess-1/run"));
    }

    [Fact]
    public async Task RunInSessionAsync_ShouldInferNonZeroExitCodeFromFinalErrorState()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            const string sse = """
data: {"type":"init","text":"sess-cmd-2","timestamp":1}

data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}

""";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(sse, Encoding.UTF8, "text/event-stream")
            });
        });
        var adapter = CreateAdapter(handler);

        var execution = await adapter.RunInSessionAsync("sess-2", "exit 7");

        execution.Id.Should().Be("sess-cmd-2");
        execution.Error.Should().NotBeNull();
        execution.Error!.Value.Should().Be("7");
        execution.Complete.Should().BeNull();
        execution.ExitCode.Should().Be(7);
    }

    [Fact]
    public async Task RunInSessionAsync_ShouldThrow_WhenSessionIdEmpty()
    {
        var adapter = CreateAdapter(new StubHttpMessageHandler((_, _) => throw new InvalidOperationException("Should not be called")));

        var act = () => adapter.RunInSessionAsync("", "echo hi");

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("*sessionId*");
    }

    [Fact]
    public async Task RunInSessionAsync_ShouldThrow_WhenCommandEmpty()
    {
        var adapter = CreateAdapter(new StubHttpMessageHandler((_, _) => throw new InvalidOperationException("Should not be called")));

        var act = () => adapter.RunInSessionAsync("sess-1", "  ");

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("*command*");
    }

    [Fact]
    public async Task DeleteSessionAsync_ShouldCallDeleteEndpoint()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Delete);
            request.RequestUri!.ToString().Should().Contain("/session/sess-to-delete");

            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK));
        });
        var adapter = CreateAdapter(handler);

        await adapter.DeleteSessionAsync("sess-to-delete");

        handler.RequestUris.Should().Contain(uri => uri.EndsWith("/session/sess-to-delete"));
    }

    [Fact]
    public async Task DeleteSessionAsync_ShouldThrow_WhenSessionIdEmpty()
    {
        var adapter = CreateAdapter(new StubHttpMessageHandler((_, _) => throw new InvalidOperationException("Should not be called")));

        var act = () => adapter.DeleteSessionAsync("  ");

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("*sessionId*");
    }

    private static CommandsAdapter CreateAdapter(HttpMessageHandler httpHandler)
    {
        var baseUrl = "http://execd.local";
        var headers = new Dictionary<string, string> { ["X-Test"] = "true" };
        var client = new HttpClientWrapper(new HttpClient(httpHandler), baseUrl, headers);
        var sseHttpClient = new HttpClient(httpHandler);
        var logger = NullLoggerFactory.Instance.CreateLogger("CommandsAdapterTests");
        return new CommandsAdapter(client, sseHttpClient, baseUrl, headers, logger);
    }

    private sealed class StubHttpMessageHandler : HttpMessageHandler
    {
        private readonly Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> _handler;

        public StubHttpMessageHandler(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> handler)
        {
            _handler = handler;
        }

        public List<string> RequestUris { get; } = new();

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            RequestUris.Add(request.RequestUri?.ToString() ?? string.Empty);
            return await _handler(request, cancellationToken).ConfigureAwait(false);
        }
    }
}
