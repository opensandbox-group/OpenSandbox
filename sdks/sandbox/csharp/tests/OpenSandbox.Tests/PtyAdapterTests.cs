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
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class PtyAdapterTests
{
    [Fact]
    public async Task CreateSessionAsync_ShouldPostAndParseSessionId()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Post);
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Created)
            {
                Content = new StringContent("{\"session_id\":\"sess-123\"}", Encoding.UTF8, "application/json")
            });
        });

        var session = await CreateAdapter(handler).CreateSessionAsync("/tmp", "bash");

        session.SessionId.Should().Be("sess-123");
        handler.RequestUris.Should().Contain(uri => uri.EndsWith("/pty"));
    }

    [Fact]
    public async Task GetSessionAsync_ShouldParseStatus()
    {
        var handler = new StubHttpMessageHandler((_, _) =>
        {
            var body = "{\"session_id\":\"sess-123\",\"running\":true,\"output_offset\":4096}";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(body, Encoding.UTF8, "application/json")
            });
        });

        var status = await CreateAdapter(handler).GetSessionAsync("sess-123");

        status.SessionId.Should().Be("sess-123");
        status.Running.Should().BeTrue();
        status.OutputOffset.Should().Be(4096);
        handler.RequestUris.Should().Contain(uri => uri.EndsWith("/pty/sess-123"));
    }

    [Fact]
    public async Task DeleteSessionAsync_ShouldIssueDelete()
    {
        var handler = new StubHttpMessageHandler((request, _) =>
        {
            request.Method.Should().Be(HttpMethod.Delete);
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK));
        });

        await CreateAdapter(handler).DeleteSessionAsync("sess-123");

        handler.RequestUris.Should().Contain(uri => uri.EndsWith("/pty/sess-123"));
    }

    private static PtyAdapter CreateAdapter(HttpMessageHandler handler)
    {
        var headers = new Dictionary<string, string>();
        var client = new HttpClientWrapper(new HttpClient(handler), "http://execd.local", headers);
        return new PtyAdapter(client);
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
