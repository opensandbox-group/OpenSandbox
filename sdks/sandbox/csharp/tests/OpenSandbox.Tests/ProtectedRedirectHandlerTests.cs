// Copyright 2026 The OpenSandbox Authors
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
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class ProtectedRedirectHandlerTests
{
    [Theory]
    [InlineData("http://EXAMPLE.test/a", "http://example.test:80/b", true)]
    [InlineData("https://example.test", "https://example.test:443/b", true)]
    [InlineData("http://bücher.test", "http://xn--bcher-kva.test", true)]
    [InlineData("http://example.test", "https://example.test", false)]
    [InlineData("http://example.test", "http://other.test", false)]
    [InlineData("http://example.test", "http://example.test:81", false)]
    public void OriginUsesSchemeHostAndEffectivePort(string source, string target, bool same)
    {
        Assert.Equal(same, ProtectedRedirectHandler.SameOrigin(new Uri(source), new Uri(target)));
    }

    [Theory]
    [InlineData(300, "POST", "GET")]
    [InlineData(301, "POST", "GET")]
    [InlineData(302, "POST", "GET")]
    [InlineData(303, "PUT", "GET")]
    [InlineData(303, "HEAD", "HEAD")]
    [InlineData(301, "PUT", "PUT")]
    [InlineData(307, "POST", "POST")]
    [InlineData(308, "PATCH", "PATCH")]
    public async Task RedirectPreservesNativeMethodAndBodyRules(int status, string method, string expected)
    {
        using var body = new StringContent("synthetic body");
        using var redirectBody = new TrackingContent();
        var calls = 0;
        using var client = Client((request, _) =>
        {
            if (calls++ == 0)
            {
                return Task.FromResult(Redirect(status, "/final", redirectBody));
            }
            Assert.Equal(expected, request.Method.Method);
            Assert.Equal(new Uri("http://example.test/final#part"), request.RequestUri);
            Assert.Same(expected == "GET" ? null : body, request.Content);
            Assert.True(redirectBody.Disposed);
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK));
        });
        using var message = new HttpRequestMessage(new HttpMethod(method), "http://example.test/start#part") { Content = body };
        using var response = await client.SendAsync(message);
        Assert.Equal(2, calls);
    }

    [Theory]
    [InlineData(304, "http://other.test")]
    [InlineData(302, "http://other.test")]
    [InlineData(302, null)]
    public async Task DoesNotFollowNonRedirectMissingLocationOrHttpsDowngrade(int status, string? location)
    {
        var calls = 0;
        using var content = new TrackingContent();
        using var client = Client((_, _) => { calls++; return Task.FromResult(Redirect(status, location, content)); });
        using var response = await client.GetAsync("https://example.test");
        Assert.Equal(1, calls);
        Assert.False(content.Disposed);
    }

    [Fact]
    public async Task RedirectLimitReturnsLastResponseAndDisposesIntermediateResponses()
    {
        var contents = new List<TrackingContent>();
        using var client = Client((_, _) =>
        {
            var content = new TrackingContent();
            contents.Add(content);
            return Task.FromResult(Redirect(302, "/again", content));
        });
        using var response = await client.GetAsync("http://example.test");
        Assert.Equal(51, contents.Count);
        Assert.All(contents.Take(50), content => Assert.True(content.Disposed));
        Assert.False(contents[50].Disposed);
        Assert.Equal(HttpStatusCode.Found, response.StatusCode);
    }

    [Fact]
    public async Task CancellationReachesRedirectedSendAndDisposesPreviousResponse()
    {
        using var cancellation = new CancellationTokenSource();
        using var content = new TrackingContent();
        var calls = 0;
        using var client = Client((_, token) =>
        {
            if (calls++ == 0) return Task.FromResult(Redirect(302, "/again", content));
            cancellation.Cancel();
            token.ThrowIfCancellationRequested();
            throw new InvalidOperationException("Expected cancellation");
        });
        await Assert.ThrowsAnyAsync<OperationCanceledException>(() => client.GetAsync("http://example.test", cancellation.Token));
        Assert.True(content.Disposed);
        Assert.Equal(2, calls);
    }

    [Fact]
    public async Task NonSeekableUploadIsNotBufferedForReplay()
    {
        using var stream = new NonSeekableStream();
        using var body = new StreamContent(stream);
        var calls = 0;
        using var client = Client(async (request, token) =>
        {
            calls++;
            await request.Content!.CopyToAsync(Stream.Null, token);
            return Redirect(307, "/again", new TrackingContent());
        });
        await Assert.ThrowsAsync<InvalidOperationException>(() => client.PostAsync("http://example.test", body));
        Assert.Equal(2, calls);
    }

    private static HttpClient Client(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> send) =>
        new(new ProtectedRedirectHandler(new StubHandler(send)));

    private static HttpResponseMessage Redirect(int status, string? location, HttpContent content) =>
        new((HttpStatusCode)status) { Content = content, Headers = { Location = location == null ? null : new Uri(location, UriKind.RelativeOrAbsolute) } };

    private sealed class StubHandler(Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>> send) : HttpMessageHandler
    {
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken token) => send(request, token);
    }

    private sealed class TrackingContent() : StringContent("")
    {
        internal bool Disposed { get; private set; }
        protected override void Dispose(bool disposing) { Disposed = true; base.Dispose(disposing); }
    }

    private sealed class NonSeekableStream() : MemoryStream(new byte[] { 1, 2, 3 })
    {
        public override bool CanSeek => false;
    }
}
