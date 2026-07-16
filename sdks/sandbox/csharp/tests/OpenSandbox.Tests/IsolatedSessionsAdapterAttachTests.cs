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
using FluentAssertions;
using Microsoft.Extensions.Logging.Abstractions;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using Xunit;

namespace OpenSandbox.Tests;

public class IsolatedSessionsAdapterAttachTests
{
    [Fact]
    public async Task AttachAsync_PopulatesFullInfo_WhenExecdReturnsAllFields()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-full")
            {
                return new RouteResponse(HttpStatusCode.OK, """
                {
                  "status": "active",
                  "created_at": "2026-01-02T03:04:05Z",
                  "last_run_at": "2026-01-02T03:05:06Z",
                  "idle_remaining_seconds": 30,
                  "profile": "strict",
                  "workspace": { "path": "/workspace", "mode": "rw" },
                  "extra_writable": ["/tmp", "/var/tmp"],
                  "binds": [{ "source": "/host/a", "dest": "/sbx/a", "readonly": true }],
                  "share_net": false,
                  "env_passthrough": { "mode": "allow", "keys": ["PATH", "HOME"] },
                  "uid": 1000,
                  "gid": 2000,
                  "uid_mode": "userns",
                  "idle_timeout_seconds": 300
                }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler, new Dictionary<string, string>
        {
            ["X-Test"] = "1"
        });

        var session = await adapter.AttachAsync("sess-full");

        handler.Requests.Should().ContainSingle();
        handler.Requests[0].Method.Should().Be(HttpMethod.Get);
        handler.Requests[0].PathAndQuery.Should().Be("/v1/isolated/session/sess-full");
        handler.Requests[0].Headers.Should().Contain("X-Test", "1");

        session.SessionId.Should().Be("sess-full");
        var info = session.Info;
        info.SessionId.Should().Be("sess-full");
        info.CreatedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:05Z"));
        info.Profile.Should().Be("strict");
        info.Workspace.Should().NotBeNull();
        info.Workspace!.Path.Should().Be("/workspace");
        info.Workspace.Mode.Should().Be("rw");
        info.ExtraWritable.Should().Equal("/tmp", "/var/tmp");
        info.Binds.Should().NotBeNull();
        info.Binds!.Should().ContainSingle();
        info.Binds![0].Source.Should().Be("/host/a");
        info.Binds![0].Dest.Should().Be("/sbx/a");
        info.Binds![0].ReadOnly.Should().Be(true);
        info.ShareNet.Should().Be(false);
        info.EnvPassthrough.Should().NotBeNull();
        info.EnvPassthrough!.Mode.Should().Be("allow");
        info.EnvPassthrough.Keys.Should().Equal("PATH", "HOME");
        info.Uid.Should().Be(1000);
        info.Gid.Should().Be(2000);
        info.UidMode.Should().Be("userns");
        info.IdleTimeoutSeconds.Should().Be(300);
    }

    [Fact]
    public async Task AttachAsync_ToleratesMissingCreationParams_WhenExecdIsOlder()
    {
        var getCallCount = 0;
        var handler = new RouteHandler(request =>
        {
            if (request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-old")
            {
                if (request.Method == HttpMethod.Get)
                {
                    getCallCount++;
                    // Older execd: only runtime status fields, no creation-parameter echoes.
                    // First GET is used by AttachAsync itself; the second GET is triggered
                    // by the returned handle's GetAsync() call and returns updated runtime state.
                    var idle = getCallCount == 1 ? "null" : "7";
                    return new RouteResponse(HttpStatusCode.OK, $$"""
                    {
                      "status": "active",
                      "created_at": "2026-01-02T03:04:05Z",
                      "last_run_at": null,
                      "idle_remaining_seconds": {{idle}}
                    }
                    """);
                }
                if (request.Method == HttpMethod.Delete)
                {
                    return new RouteResponse(HttpStatusCode.NoContent, "");
                }
            }
            if (request.Method == HttpMethod.Post &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/sess-old/run")
            {
                // SSE stream with one complete event so RunAsync succeeds.
                return new RouteResponse(
                    HttpStatusCode.OK,
                    "event: complete\ndata: {\"type\":\"complete\"}\n\n",
                    "text/event-stream");
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var session = await adapter.AttachAsync("sess-old");

        var info = session.Info;
        info.SessionId.Should().Be("sess-old");
        info.CreatedAt.Should().Be(DateTimeOffset.Parse("2026-01-02T03:04:05Z"));
        info.Profile.Should().BeNull();
        info.Workspace.Should().BeNull();
        info.ExtraWritable.Should().BeNull();
        info.Binds.Should().BeNull();
        info.ShareNet.Should().BeNull();
        info.EnvPassthrough.Should().BeNull();
        info.Uid.Should().BeNull();
        info.Gid.Should().BeNull();
        info.UidMode.Should().BeNull();
        info.IdleTimeoutSeconds.Should().BeNull();

        // The returned handle must still work — RunAsync/GetAsync/DeleteAsync only need sessionId.
        var runResult = await session.RunAsync("echo ok");
        runResult.Should().NotBeNull();

        var state = await session.GetAsync();
        state.Status.Should().Be("active");
        state.IdleRemainingSeconds.Should().Be(7);

        await session.DeleteAsync();

        handler.Requests.Select(r => r.Method).Should().Equal(
            HttpMethod.Get,   // attach
            HttpMethod.Post,  // run
            HttpMethod.Get,   // get
            HttpMethod.Delete // delete
        );
    }

    [Fact]
    public async Task AttachAsync_ThrowsNotFound_WhenSessionMissing()
    {
        var handler = new RouteHandler(request =>
        {
            if (request.Method == HttpMethod.Get &&
                request.RequestUri!.PathAndQuery == "/v1/isolated/session/missing-sess")
            {
                return new RouteResponse(HttpStatusCode.NotFound, """
                { "code": "SESSION_NOT_FOUND", "message": "isolated session not found" }
                """);
            }
            return new RouteResponse(HttpStatusCode.InternalServerError, "wrong endpoint");
        });
        var adapter = CreateAdapter(handler);

        var ex = await Assert.ThrowsAsync<SandboxApiException>(
            () => adapter.AttachAsync("missing-sess"));

        ex.StatusCode.Should().Be(404);
        ex.Message.Should().Contain("attach isolated session");
        ex.Message.Should().Contain("SESSION_NOT_FOUND");
    }

    [Fact]
    public async Task AttachAsync_RejectsBlankSessionId()
    {
        var handler = new RouteHandler(_ => new RouteResponse(HttpStatusCode.OK, "{}"));
        var adapter = CreateAdapter(handler);

        await Assert.ThrowsAsync<ArgumentException>(() => adapter.AttachAsync(""));
        await Assert.ThrowsAsync<ArgumentException>(() => adapter.AttachAsync("   "));
        handler.Requests.Should().BeEmpty();
    }

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
            NullLoggerFactory.Instance.CreateLogger("IsolatedSessionsAdapterAttachTests"));
    }

    private sealed record CapturedRequest(
        HttpMethod Method,
        string? PathAndQuery,
        IReadOnlyDictionary<string, string> Headers,
        string? Body);

    private sealed record RouteResponse(
        HttpStatusCode Status,
        string Body,
        string ContentType = "application/json");

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
            return new HttpResponseMessage(response.Status)
            {
                Content = new StringContent(response.Body, Encoding.UTF8, response.ContentType)
            };
        }
    }
}
