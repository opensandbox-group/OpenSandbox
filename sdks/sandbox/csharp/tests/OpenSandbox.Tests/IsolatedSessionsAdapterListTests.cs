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
using Xunit;

namespace OpenSandbox.Tests;

public class IsolatedSessionsAdapterListTests
{
    [Fact]
    public async Task ListAsync_ShouldParsePopulatedSessions()
    {
        var handler = new CaptureHandler(_ => """
        {
          "sessions": [
            {
              "session_id": "sess-1",
              "status": "running",
              "created_at": "2026-01-01T00:00:00Z",
              "last_run_at": "2026-01-01T00:05:00Z",
              "idle_remaining_seconds": 120
            },
            {
              "session_id": "sess-2",
              "status": "idle",
              "created_at": "2026-01-01T00:01:00Z",
              "last_run_at": null,
              "idle_remaining_seconds": null
            }
          ]
        }
        """);
        var adapter = CreateAdapter(handler, new Dictionary<string, string>
        {
            ["X-Global"] = "global"
        });

        var sessions = await adapter.ListAsync();

        handler.Requests.Should().ContainSingle();
        var request = handler.Requests[0];
        request.Method.Should().Be(HttpMethod.Get);
        request.PathAndQuery.Should().Be("/v1/isolated/sessions");
        request.Headers.Should().Contain("X-Global", "global");

        sessions.Should().HaveCount(2);
        sessions[0].SessionId.Should().Be("sess-1");
        sessions[0].Status.Should().Be("running");
        sessions[0].CreatedAt.Should().Be(DateTimeOffset.Parse("2026-01-01T00:00:00Z"));
        sessions[0].LastRunAt.Should().Be(DateTimeOffset.Parse("2026-01-01T00:05:00Z"));
        sessions[0].IdleRemainingSeconds.Should().Be(120);
        sessions[1].SessionId.Should().Be("sess-2");
        sessions[1].LastRunAt.Should().BeNull();
        sessions[1].IdleRemainingSeconds.Should().BeNull();
    }

    [Fact]
    public async Task ListAsync_ShouldReturnEmptyForEmptyArray()
    {
        var handler = new CaptureHandler(_ => """{ "sessions": [] }""");
        var adapter = CreateAdapter(handler);

        var sessions = await adapter.ListAsync();

        sessions.Should().BeEmpty();
    }

    [Fact]
    public async Task ListAsync_ShouldReturnEmptyForMissingArray()
    {
        var handler = new CaptureHandler(_ => "{}");
        var adapter = CreateAdapter(handler);

        var sessions = await adapter.ListAsync();

        sessions.Should().BeEmpty();
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
            NullLoggerFactory.Instance.CreateLogger("IsolatedSessionsAdapterListTests"));
    }

    private sealed class CaptureHandler(Func<HttpRequestMessage, string> payloadSelector) : HttpMessageHandler
    {
        public List<CapturedRequest> Requests { get; } = [];

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var body = request.Content == null
                ? null
                : await request.Content.ReadAsStringAsync().ConfigureAwait(false);
            Requests.Add(new CapturedRequest(
                request.Method,
                request.RequestUri?.PathAndQuery,
                request.Headers.ToDictionary(header => header.Key, header => string.Join(",", header.Value)),
                body));

            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payloadSelector(request), Encoding.UTF8, "application/json")
            };
            return response;
        }
    }

    private sealed record CapturedRequest(
        HttpMethod Method,
        string? PathAndQuery,
        IReadOnlyDictionary<string, string> Headers,
        string? Body);
}
