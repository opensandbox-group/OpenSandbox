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
using Microsoft.Extensions.Logging.Abstractions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class IsolatedSessionsAdapterBackgroundTests
{
    [Fact]
    public async Task RunBackgroundAsync_PostsBackgroundFlagWithoutTimeout()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/run")
            {
                return new RouteResponse(HttpStatusCode.Accepted, """
                {
                  "session_id": "sess-1",
                  "run_id": "run-1",
                  "started_at": "2026-01-02T03:04:05Z"
                }
                """);
            }
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session")
            {
                return new RouteResponse(HttpStatusCode.OK, CreateSessionResponse);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var session = await adapter.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));

        var run = await session.RunBackgroundAsync(
            "echo hi",
            new IsolatedRunOpts(
                new Dictionary<string, string> { ["A"] = "b" },
                TimeoutSeconds: 30));

        handler.Requests.Should().HaveCount(2);
        var request = handler.Requests[1];
        request.Method.Should().Be(HttpMethod.Post);
        request.PathAndQuery.Should().Be("/v1/isolated/session/sess-1/run");

        var body = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(request.Body!);
        body!["code"].GetString().Should().Be("echo hi");
        body["background"].GetBoolean().Should().BeTrue();
        body["envs"].GetProperty("A").GetString().Should().Be("b");
        body.Should().NotContainKey("timeout_seconds");

        run.SessionId.Should().Be("sess-1");
        run.RunId.Should().Be("run-1");
        run.StartedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:05Z"));
    }

    [Fact]
    public async Task RunBackgroundAsync_OmitsEnvsWhenNotProvided()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/run")
            {
                return new RouteResponse(HttpStatusCode.Accepted, """
                { "session_id": "sess-1", "run_id": "run-1" }
                """);
            }
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session")
            {
                return new RouteResponse(HttpStatusCode.OK, CreateSessionResponse);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var session = await adapter.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        await session.RunBackgroundAsync("echo hi");

        var body = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(handler.Requests[1].Body!);
        body!.Keys.Should().Equal("code", "background");
    }

    [Fact]
    public async Task RunBackgroundAsync_ThrowsApiError_WhenNotAccepted()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/run")
            {
                return new RouteResponse(HttpStatusCode.NotFound, """
                { "code": "SESSION_NOT_FOUND", "message": "isolated session not found" }
                """);
            }
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session")
            {
                return new RouteResponse(HttpStatusCode.OK, CreateSessionResponse);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var session = await adapter.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));

        var ex = await Assert.ThrowsAsync<SandboxApiException>(
            () => session.RunBackgroundAsync("echo hi"));

        ex.StatusCode.Should().Be(404);
        ex.Message.Should().Contain("run background in isolated session");
        ex.Message.Should().Contain("SESSION_NOT_FOUND");
    }

    [Fact]
    public async Task GetRunStatusAsync_ParsesRunningStatus()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1")
            {
                return new RouteResponse(HttpStatusCode.OK, """
                {
                  "session_id": "sess-1",
                  "run_id": "run-1",
                  "running": true,
                  "started_at": "2026-01-02T03:04:05Z"
                }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var status = await adapter.RunStatusInternalAsync("sess-1", "run-1");

        handler.Requests.Should().ContainSingle();
        handler.Requests[0].Method.Should().Be(HttpMethod.Get);
        handler.Requests[0].PathAndQuery.Should().Be("/v1/isolated/session/sess-1/runs/run-1");

        status.SessionId.Should().Be("sess-1");
        status.RunId.Should().Be("run-1");
        status.Running.Should().BeTrue();
        status.ExitCode.Should().BeNull();
        status.Error.Should().BeNull();
        status.StartedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:05Z"));
        status.FinishedAt.Should().BeNull();
    }

    [Fact]
    public async Task GetRunStatusAsync_ParsesFinishedStatus()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1")
            {
                return new RouteResponse(HttpStatusCode.OK, """
                {
                  "session_id": "sess-1",
                  "run_id": "run-1",
                  "running": false,
                  "exit_code": 7,
                  "started_at": "2026-01-02T03:04:05Z",
                  "finished_at": "2026-01-02T03:04:09Z"
                }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var status = await adapter.RunStatusInternalAsync("sess-1", "run-1");

        status.Running.Should().BeFalse();
        status.ExitCode.Should().Be(7);
        status.StartedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:05Z"));
        status.FinishedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:09Z"));
    }

    [Fact]
    public async Task GetRunStatusAsync_ParsesTerminatedWithError()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1")
            {
                return new RouteResponse(HttpStatusCode.OK, """
                {
                  "session_id": "sess-1",
                  "run_id": "run-1",
                  "running": false,
                  "error": "session terminated"
                }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var status = await adapter.RunStatusInternalAsync("sess-1", "run-1");

        status.Running.Should().BeFalse();
        status.ExitCode.Should().BeNull();
        status.Error.Should().Be("session terminated");
    }

    [Fact]
    public async Task GetRunStatusAsync_ThrowsApiError_WhenRunMissing()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/missing-run")
            {
                return new RouteResponse(HttpStatusCode.NotFound, """
                { "code": "RUN_NOT_FOUND", "message": "background run not found" }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var ex = await Assert.ThrowsAsync<SandboxApiException>(
            () => adapter.RunStatusInternalAsync("sess-1", "missing-run"));

        ex.StatusCode.Should().Be(404);
        ex.Message.Should().Contain("get isolated run status");
        ex.Message.Should().Contain("RUN_NOT_FOUND");
    }

    [Fact]
    public async Task GetRunLogsAsync_UsesHeaderCursor()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1/logs?cursor=4")
            {
                var response = new RouteResponse(HttpStatusCode.OK, "line1\nline2\n", "text/plain");
                response.Headers["EXECD-ISOLATED-TAIL-CURSOR"] = "12";
                return response;
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var logs = await adapter.RunLogsInternalAsync("sess-1", "run-1", cursor: 4);

        handler.Requests.Should().ContainSingle();
        handler.Requests[0].Method.Should().Be(HttpMethod.Get);
        handler.Requests[0].PathAndQuery.Should().Be("/v1/isolated/session/sess-1/runs/run-1/logs?cursor=4");

        logs.Text.Should().Be("line1\nline2\n");
        logs.Cursor.Should().Be(12);
    }

    [Fact]
    public async Task GetRunLogsAsync_OmitsCursorQuery_WhenCursorIsZero()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1/logs")
            {
                return new RouteResponse(HttpStatusCode.OK, "hello", "text/plain");
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var logs = await adapter.RunLogsInternalAsync("sess-1", "run-1");

        handler.Requests.Should().ContainSingle();
        handler.Requests[0].PathAndQuery.Should().Be("/v1/isolated/session/sess-1/runs/run-1/logs");

        // No header: cursor advances by the bytes actually returned.
        logs.Text.Should().Be("hello");
        logs.Cursor.Should().Be(5);
    }

    [Fact]
    public async Task GetRunLogsAsync_FallsBackToRequestedCursorPlusBytes_WhenHeaderInvalid()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1/logs?cursor=4")
            {
                var response = new RouteResponse(HttpStatusCode.OK, "hello", "text/plain");
                response.Headers["EXECD-ISOLATED-TAIL-CURSOR"] = "not-a-number";
                return response;
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var logs = await adapter.RunLogsInternalAsync("sess-1", "run-1", cursor: 4);

        logs.Text.Should().Be("hello");
        logs.Cursor.Should().Be(9);
    }

    [Fact]
    public async Task GetRunLogsAsync_CountsUtf8Bytes()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/run-1/logs")
            {
                return new RouteResponse(HttpStatusCode.OK, "héllo", "text/plain");
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var logs = await adapter.RunLogsInternalAsync("sess-1", "run-1");

        logs.Text.Should().Be("héllo");
        logs.Cursor.Should().Be(6);
    }

    [Fact]
    public async Task GetRunLogsAsync_ThrowsApiError_WhenRunMissing()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-1/runs/missing-run/logs")
            {
                return new RouteResponse(HttpStatusCode.NotFound, """
                { "code": "RUN_NOT_FOUND", "message": "background run not found" }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var ex = await Assert.ThrowsAsync<SandboxApiException>(
            () => adapter.RunLogsInternalAsync("sess-1", "missing-run"));

        ex.StatusCode.Should().Be(404);
        ex.Message.Should().Contain("get isolated run logs");
    }

    [Fact]
    public async Task BackgroundMethods_RejectBlankOrNegativeInputs()
    {
        var handler = new RouteHandler(_ => new RouteResponse(HttpStatusCode.OK, "{}"));
        var adapter = CreateAdapter(handler);

        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunBackgroundInternalAsync("", "echo hi"));
        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunBackgroundInternalAsync("sess-1", ""));
        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunBackgroundInternalAsync("sess-1", "  "));
        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunStatusInternalAsync("", "run-1"));
        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunStatusInternalAsync("sess-1", ""));
        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.RunLogsInternalAsync("sess-1", "run-1", cursor: -1));
        handler.Requests.Should().BeEmpty();
    }

    private const string CreateSessionResponse =
        """{ "session_id": "sess-1", "created_at": "2026-01-02T03:04:05Z" }""";

    private static IsolatedSessionsAdapter CreateAdapter(
        HttpMessageHandler handler,
        IReadOnlyDictionary<string, string>? headers = null)
    {
        var client = new HttpClient(handler);
        var sseClient = new HttpClient(handler);
        return new IsolatedSessionsAdapter(
            client,
            sseClient,
            "http://execd.local",
            headers ?? new Dictionary<string, string>(),
            NullLoggerFactory.Instance.CreateLogger("IsolatedSessionsAdapterBackgroundTests"));
    }

    private sealed record CapturedRequest(
        HttpMethod Method,
        string? PathAndQuery,
        IReadOnlyDictionary<string, string> Headers,
        string? Body);

    private sealed record RouteResponse(
        HttpStatusCode Status,
        string Body,
        string ContentType = "application/json")
    {
        public Dictionary<string, string> Headers { get; } = new();
    }

    private sealed class RouteHandler : HttpMessageHandler
    {
        private readonly Func<HttpRequestMessage, RouteResponse> _router;
        public List<CapturedRequest> Requests { get; } = [];

        public RouteHandler(Func<HttpRequestMessage, RouteResponse> router)
        {
            _router = router;
        }

        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var body = request.Content == null
                ? null
                : await request.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
            Requests.Add(new CapturedRequest(
                request.Method,
                request.RequestUri?.PathAndQuery,
                request.Headers.ToDictionary(h => h.Key, h => string.Join(",", h.Value)),
                body));

            var response = _router(request);
            var result = new HttpResponseMessage(response.Status)
            {
                Content = new StringContent(response.Body, Encoding.UTF8, response.ContentType)
            };
            foreach (var header in response.Headers)
            {
                result.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
            return result;
        }
    }
}
