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
using System.Net.Sockets;
using System.Text;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class RedirectCredentialTests
{
    private static readonly string[] ProtectedHeaders =
    [
        Constants.ApiKeyHeader, "oPeN-sAnDbOx-DeFaUlT", "oPeNsAnDbOx-DeFaUlT",
        "X-API-Key", "x-ExEcD-aCcEsS-ToKeN", "OpenSandbox-Secure-Access", "oPeN-sAnDbOx-ReQuEsT", "oPeNsAnDbOx-ReQuEsT"
    ];

    [Theory]
    [InlineData(false, false, false)]
    [InlineData(true, false, false)]
    [InlineData(false, true, false)]
    [InlineData(true, true, false)]
    [InlineData(false, false, true)]
    [InlineData(true, false, true)]
    [InlineData(false, true, true)]
    [InlineData(true, true, true)]
    public async Task Redirect_StripsProtectedHeadersOnlyWhenOriginChanges(bool sse, bool crossOrigin, bool sync)
    {
        await using var target = new LoopbackServer(_ => Ok);
        await using var source = new LoopbackServer(request => request.Path == "/start"
            ? Redirect(crossOrigin ? target.Url + "/final" : "/final") : Ok, crossOrigin ? 1 : 2);
        var config = Config(source.Url);
        using var client = sse ? config.CreateSseHttpClient() : config.CreateHttpClient();
        using var request = Request(source.Url + "/start");
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));

        using var response = sync ? client.Send(request, timeout.Token) : await client.SendAsync(request, timeout.Token);

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        var sent = await source.Captured;
        var received = crossOrigin ? Assert.Single(await target.Captured) : sent[1];
        AssertProtectedHeaders(sent[0], present: true);
        AssertProtectedHeaders(received, present: !crossOrigin);
        Assert.Equal("trace-test", received.Headers["X-Request-ID"]);
        // Preserve HttpClient's existing handling of these standard headers.
        Assert.False(received.Headers.ContainsKey("Authorization"));
        Assert.Equal("explicit=test", received.Headers["Cookie"]);
        Assert.Equal("tenant-test", Assert.Single(client.DefaultRequestHeaders.GetValues(Constants.ApiKeyHeader)));
        Assert.Equal(sse ? Timeout.InfiniteTimeSpan : TimeSpan.FromSeconds(7), client.Timeout);
    }

    [Theory]
    [InlineData(false, false)]
    [InlineData(true, false)]
    [InlineData(false, true)]
    [InlineData(true, true)]
    public async Task Redirect_LeavingAndReturningToOrigin_DoesNotRestoreCredentials(bool sse, bool sync)
    {
        string? sourceUrl = null;
        await using var target = new LoopbackServer(_ => Redirect(sourceUrl + "/returned"));
        await using var source = new LoopbackServer(request => request.Path == "/start"
            ? Redirect(target.Url + "/away") : Ok, 2);
        sourceUrl = source.Url;
        var config = Config(source.Url);
        using var client = sse ? config.CreateSseHttpClient() : config.CreateHttpClient();
        using var request = Request(source.Url + "/start");
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));

        using var response = sync ? client.Send(request, timeout.Token) : await client.SendAsync(request, timeout.Token);

        Assert.Equal(HttpStatusCode.OK, response.StatusCode);
        var sent = await source.Captured;
        AssertProtectedHeaders(sent[0], present: true);
        AssertProtectedHeaders(Assert.Single(await target.Captured), present: false);
        AssertProtectedHeaders(sent[1], present: false);
        Assert.Equal("trace-test", sent[1].Headers["X-Request-ID"]);
    }

    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task LifecycleMetrics_Redirect_ProtectsCredentialsAndPreservesPost(bool crossOrigin)
    {
        await using var target = new LoopbackServer(_ => Ok);
        await using var source = new LoopbackServer(request => request.Path == "/v1/metrics/events"
            ? Redirect(crossOrigin ? target.Url + "/final" : "/final") : Ok, crossOrigin ? 1 : 2);
        var config = Config(source.Url);
        // Metrics uses its own client and copies all configured headers onto the request.
        var headers = new Dictionary<string, string>(config.Headers)
        {
            ["x-ExEcD-aCcEsS-ToKeN"] = "endpoint-test",
            ["oPeN-sAnDbOx-ReQuEsT"] = "request-test",
            ["oPeNsAnDbOx-ReQuEsT"] = "request-test"
        };
        config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = source.Url,
            ApiKey = "tenant-test",
            Headers = headers
        });

        LifecycleMetricsReporter.ReportSandboxCreate(config, "sandbox-test", "image-test", 12, true);

        var sent = await source.Captured;
        var received = crossOrigin ? Assert.Single(await target.Captured) : sent[1];
        AssertProtectedHeaders(sent[0], present: true);
        AssertProtectedHeaders(received, present: !crossOrigin);
        Assert.Equal("POST", received.Method);
        Assert.Equal(sent[0].Body, received.Body);
        Assert.Contains("sandbox.create", received.Body);
        Assert.Equal("trace-test", received.Headers["X-Request-ID"]);
    }

    [Fact]
    public async Task Redirect_AlsoStripsProtectedContentHeadersWithoutDroppingBody()
    {
        await using var target = new LoopbackServer(_ => Ok);
        await using var source = new LoopbackServer(_ => Redirect(target.Url + "/final"));
        using var client = Config(source.Url).CreateHttpClient();
        using var body = new StringContent("synthetic upload");
        body.Headers.TryAddWithoutValidation("X-EXECD-ACCESS-TOKEN", "content-token");
        body.Headers.TryAddWithoutValidation("OPEN-SANDBOX-Content", "content-token");
        body.Headers.TryAddWithoutValidation("Content-Language", "en");
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        using var response = await client.PostAsync(source.Url, body, timeout.Token);

        var original = Assert.Single(await source.Captured);
        var redirected = Assert.Single(await target.Captured);
        Assert.Equal("content-token", original.Headers["X-EXECD-ACCESS-TOKEN"]);
        Assert.False(redirected.Headers.ContainsKey("X-EXECD-ACCESS-TOKEN"));
        Assert.False(redirected.Headers.ContainsKey("OPEN-SANDBOX-Content"));
        Assert.Equal(original.Body, redirected.Body);
        Assert.Equal("en", redirected.Headers["Content-Language"]);
    }

    private static ConnectionConfig Config(string url) => new(new ConnectionConfigOptions
    {
        Domain = url,
        ApiKey = "tenant-test",
        RequestTimeoutSeconds = 7,
        Headers = new Dictionary<string, string>
        {
            ["oPeN-sAnDbOx-DeFaUlT"] = "default-test",
            ["oPeNsAnDbOx-DeFaUlT"] = "default-test",
            ["X-API-Key"] = "api-test",
            ["OpenSandbox-Secure-Access"] = "secure-access-test",
            ["X-Request-ID"] = "trace-test"
        }
    });

    private static HttpRequestMessage Request(string url)
    {
        var request = new HttpRequestMessage(HttpMethod.Get, url);
        request.Headers.TryAddWithoutValidation("x-ExEcD-aCcEsS-ToKeN", "endpoint-test");
        request.Headers.TryAddWithoutValidation("oPeN-sAnDbOx-ReQuEsT", "request-test");
        request.Headers.TryAddWithoutValidation("oPeNsAnDbOx-ReQuEsT", "request-test");
        request.Headers.TryAddWithoutValidation("Authorization", "Bearer synthetic-test");
        request.Headers.TryAddWithoutValidation("Cookie", "explicit=test");
        return request;
    }

    private static void AssertProtectedHeaders(CapturedRequest request, bool present)
    {
        foreach (var header in ProtectedHeaders)
        {
            Assert.Equal(present, request.Headers.ContainsKey(header));
        }
        if (!present)
        {
            Assert.DoesNotContain(request.Headers.Keys, name =>
                name.StartsWith("OPEN-SANDBOX-", StringComparison.OrdinalIgnoreCase) ||
                name.StartsWith("OPENSANDBOX-", StringComparison.OrdinalIgnoreCase));
        }
    }

    private const string Ok = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";

    private static string Redirect(string location) =>
        $"HTTP/1.1 307 Temporary Redirect\r\nLocation: {location}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";

    private sealed record CapturedRequest(string Method, string Path, Dictionary<string, string> Headers, string Body);

    // Bounded, loopback-only HTTP/1.1 fixture. All test bodies are synthetic ASCII with Content-Length.
    private sealed class LoopbackServer : IAsyncDisposable
    {
        private readonly TcpListener _listener = new(IPAddress.Loopback, 0);
        private readonly CancellationTokenSource _stop = new(TimeSpan.FromSeconds(10));
        private readonly TaskCompletionSource<IReadOnlyList<CapturedRequest>> _captured =
            new(TaskCreationOptions.RunContinuationsAsynchronously);
        private readonly Task _serving;

        public string Url { get; }
        public Task<IReadOnlyList<CapturedRequest>> Captured => _captured.Task.WaitAsync(TimeSpan.FromSeconds(5));

        public LoopbackServer(Func<CapturedRequest, string> response, int count = 1)
        {
            _listener.Start();
            Url = $"http://127.0.0.1:{((IPEndPoint)_listener.LocalEndpoint).Port}";
            _serving = ServeAsync(response, count);
        }

        private async Task ServeAsync(Func<CapturedRequest, string> response, int count)
        {
            try
            {
                var requests = new List<CapturedRequest>();
                for (int i = 0; i < count; i++)
                {
                    using var client = await _listener.AcceptTcpClientAsync(_stop.Token);
                    using var stream = client.GetStream();
                    using var reader = new StreamReader(stream, Encoding.ASCII, false, 1024, leaveOpen: true);
                    var line = await reader.ReadLineAsync(_stop.Token) ?? throw new IOException("Missing request line");
                    var parts = line.Split(' ');
                    var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                    while (!string.IsNullOrEmpty(line = await reader.ReadLineAsync(_stop.Token)))
                    {
                        var colon = line.IndexOf(':');
                        headers.Add(line[..colon], line[(colon + 1)..].Trim());
                    }
                    var length = headers.TryGetValue("Content-Length", out var value) ? int.Parse(value) : 0;
                    var body = new char[length];
                    int offset = 0;
                    while (offset < body.Length)
                    {
                        var read = await reader.ReadAsync(body.AsMemory(offset), _stop.Token);
                        if (read == 0)
                        {
                            throw new IOException("Incomplete request body");
                        }
                        offset += read;
                    }
                    var request = new CapturedRequest(parts[0], parts[1], headers, new string(body));
                    requests.Add(request);
                    await stream.WriteAsync(Encoding.ASCII.GetBytes(response(request)), _stop.Token);
                }
                _captured.TrySetResult(requests);
            }
            catch (Exception ex)
            {
                _captured.TrySetException(ex);
            }
        }

        public async ValueTask DisposeAsync()
        {
            _stop.Cancel();
            _listener.Stop();
            await _serving;
            // Observe cancellation when an optional target origin was unused.
            _ = _captured.Task.Exception;
            _stop.Dispose();
        }
    }
}
