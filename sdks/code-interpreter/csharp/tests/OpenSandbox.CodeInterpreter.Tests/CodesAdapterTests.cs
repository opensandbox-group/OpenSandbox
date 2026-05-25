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
using System.Net.Http.Headers;
using System.Text;
using OpenSandbox.CodeInterpreter.Adapters;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class CodesAdapterTests
{
    [Fact]
    public async Task ListContextsAsync_ThrowsOnEmptyLanguage()
    {
        var adapter = CreateAdapter(
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))),
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))));

        await Assert.ThrowsAsync<InvalidArgumentException>(() => adapter.ListContextsAsync(" "));
    }

    [Fact]
    public async Task ListContextsAsync_SendsLanguageQuery()
    {
        var httpHandler = new StubHttpMessageHandler((request, _) =>
        {
            var body = "[{\"id\":\"ctx-1\",\"language\":\"python\"}]";
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(body, Encoding.UTF8, "application/json")
            };
            return Task.FromResult(response);
        });

        var adapter = CreateAdapter(
            httpHandler,
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))));

        var contexts = await adapter.ListContextsAsync("python");

        Assert.Single(contexts);
        Assert.Equal("python", contexts[0].Language);
        Assert.Contains(httpHandler.RequestUris, uri => uri.Contains("/code/contexts?language=python", StringComparison.Ordinal));
    }

    [Fact]
    public async Task RunStreamAsync_ThrowsOnEmptyCode()
    {
        var adapter = CreateAdapter(
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))),
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))));

        var request = new RunCodeRequest
        {
            Code = "   ",
            Context = new CodeContext { Language = SupportedLanguage.Python }
        };

        await Assert.ThrowsAsync<InvalidArgumentException>(() => DrainAsync(adapter.RunStreamAsync(request)));
    }

    [Fact]
    public async Task RunStreamAsync_ParsesSseEvent()
    {
        var sseHandler = new StubHttpMessageHandler((request, _) =>
        {
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(
                    "data: {\"type\":\"stdout\",\"text\":\"hello\",\"timestamp\":1}\n\n",
                    Encoding.UTF8,
                    "text/event-stream")
            };
            return Task.FromResult(response);
        });

        var adapter = CreateAdapter(
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))),
            sseHandler);

        var request = new RunCodeRequest
        {
            Code = "print('hello')",
            Context = new CodeContext { Language = SupportedLanguage.Python }
        };

        var events = new List<ServerStreamEvent>();
        await foreach (var ev in adapter.RunStreamAsync(request))
        {
            events.Add(ev);
        }

        Assert.Single(events);
        Assert.Equal(ServerStreamEventTypes.Stdout, events[0].Type);
        Assert.Equal("hello", events[0].Text);
        Assert.Contains(sseHandler.AcceptHeaders, value => value.Contains("text/event-stream", StringComparison.OrdinalIgnoreCase));
    }

    [Fact]
    public async Task InterruptAsync_SendsExecutionIdAsQueryParameter()
    {
        var httpHandler = new StubHttpMessageHandler((request, _) =>
            Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK)));

        var adapter = CreateAdapter(
            httpHandler,
            new StubHttpMessageHandler((_, _) => Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK))));

        await adapter.InterruptAsync("exec-123");

        Assert.Contains(httpHandler.RequestUris, uri => uri.Contains("/code?id=exec-123", StringComparison.Ordinal));
    }

    private static async Task DrainAsync<T>(IAsyncEnumerable<T> source)
    {
        await foreach (var _ in source)
        {
        }
    }

    private static CodesAdapter CreateAdapter(HttpMessageHandler httpHandler, HttpMessageHandler sseHandler)
    {
        var baseUrl = "http://execd.local";
        var headers = new Dictionary<string, string> { ["X-Test"] = "true" };
        var client = new HttpClientWrapper(new HttpClient(httpHandler), baseUrl, headers);
        var sseHttpClient = new HttpClient(sseHandler);
        var logger = NullLoggerFactory.Instance.CreateLogger("CodesAdapterTests");
        return new CodesAdapter(client, sseHttpClient, baseUrl, headers, logger);
    }

    private sealed class StubHttpMessageHandler : HttpMessageHandler
    {
        private readonly Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> _handler;

        public StubHttpMessageHandler(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> handler)
        {
            _handler = handler;
        }

        public List<string> RequestUris { get; } = new();
        public List<string> AcceptHeaders { get; } = new();

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            RequestUris.Add(request.RequestUri?.ToString() ?? string.Empty);
            AcceptHeaders.Add(string.Join(",", request.Headers.Accept.Select(MediaTypeToString)));
            return await _handler(request, cancellationToken).ConfigureAwait(false);
        }

        private static string MediaTypeToString(MediaTypeWithQualityHeaderValue value)
        {
            return value.MediaType ?? string.Empty;
        }
    }
}
