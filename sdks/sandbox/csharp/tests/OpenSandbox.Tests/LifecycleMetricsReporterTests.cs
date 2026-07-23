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
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class LifecycleMetricsReporterTests
{
    [Fact]
    public void BuildPayload_OmitsOptionalFields_WhenNullOrEmpty()
    {
        var payload = LifecycleMetricsReporter.BuildPayload(
            sandboxId: null,
            image: null,
            createDurationMs: 12,
            success: false);

        payload["eventType"].Should().Be("sandbox.create");
        payload["createDurationMs"].Should().Be(12L);
        payload["success"].Should().Be(false);
        payload.Should().NotContainKey("sandboxId");
        payload.Should().NotContainKey("image");
    }

    [Fact]
    public void BuildPayload_ClampsNegativeDurationToZero()
    {
        var payload = LifecycleMetricsReporter.BuildPayload(
            sandboxId: "sbx-1",
            image: "python:3.12",
            createDurationMs: -42,
            success: true);

        payload["createDurationMs"].Should().Be(0L);
        payload["sandboxId"].Should().Be("sbx-1");
        payload["image"].Should().Be("python:3.12");
    }

    [Fact]
    public void ReportSandboxCreate_DoesNotThrow_WhenServerUnreachable()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            // Use an address that refuses connections quickly.
            Domain = "127.0.0.1:1",
            Protocol = ConnectionProtocol.Http,
            RequestTimeoutSeconds = 1,
        });

        // Fire-and-forget; must not raise or block callers meaningfully.
        Action act = () => LifecycleMetricsReporter.ReportSandboxCreate(
            config,
            sandboxId: "sbx",
            image: "python:3.12",
            createDurationMs: 5,
            success: false);

        act.Should().NotThrow();
    }

    [Fact]
    public void ReportSandboxCreate_SkipsPost_WhenDisableMetricsFlagSet()
    {
        using var server = new StubHttpServer();
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = $"127.0.0.1:{server.Port}",
            Protocol = ConnectionProtocol.Http,
            RequestTimeoutSeconds = 2,
            DisableMetrics = true,
        });

        LifecycleMetricsReporter.ReportSandboxCreate(
            config,
            sandboxId: "sbx",
            image: "python:3.12",
            createDurationMs: 10,
            success: true);

        server.WaitForRequest(TimeSpan.FromMilliseconds(200)).Should().BeFalse(
            "metrics must not be posted when DisableMetrics=true");
    }

    [Fact]
    public async Task ReportSandboxCreate_PostsExpectedPayload()
    {
        using var server = new StubHttpServer();
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = $"127.0.0.1:{server.Port}",
            Protocol = ConnectionProtocol.Http,
            RequestTimeoutSeconds = 5,
            ApiKey = "test-key",
        });

        LifecycleMetricsReporter.ReportSandboxCreate(
            config,
            sandboxId: "sbx",
            image: "python:3.12",
            createDurationMs: 42,
            success: true);

        var received = await server.WaitForRequestAsync(TimeSpan.FromSeconds(3));
        received.Should().NotBeNull();
        received!.RequestLine.Should().StartWith("POST /v1/metrics/events ");

        var body = JsonSerializer.Deserialize<JsonElement>(received.Body);
        body.GetProperty("eventType").GetString().Should().Be("sandbox.create");
        body.GetProperty("createDurationMs").GetInt64().Should().Be(42);
        body.GetProperty("success").GetBoolean().Should().BeTrue();
        body.GetProperty("sandboxId").GetString().Should().Be("sbx");
        body.GetProperty("image").GetString().Should().Be("python:3.12");

        received.Headers.Should().ContainKey(Constants.ApiKeyHeader);
        received.Headers[Constants.ApiKeyHeader].Should().Be("test-key");
    }

    /// <summary>
    /// Tiny single-shot HTTP stub that accepts one request and captures the request line, headers, and body.
    /// Only intended for these fire-and-forget metric tests.
    /// </summary>
    private sealed class StubHttpServer : IDisposable
    {
        private readonly TcpListener _listener;
        private readonly TaskCompletionSource<StubHttpRequest> _tcs = new();
        private readonly CancellationTokenSource _cts = new();

        public int Port { get; }

        public StubHttpServer()
        {
            _listener = new TcpListener(IPAddress.Loopback, 0);
            _listener.Start();
            Port = ((IPEndPoint)_listener.LocalEndpoint).Port;
            _ = Task.Run(() => AcceptAsync(_cts.Token));
        }

        public bool WaitForRequest(TimeSpan timeout)
        {
            return _tcs.Task.Wait(timeout);
        }

        public async Task<StubHttpRequest?> WaitForRequestAsync(TimeSpan timeout)
        {
            var completed = await Task.WhenAny(_tcs.Task, Task.Delay(timeout)).ConfigureAwait(false);
            return completed == _tcs.Task ? await _tcs.Task.ConfigureAwait(false) : null;
        }

        private async Task AcceptAsync(CancellationToken cancellationToken)
        {
            try
            {
                using var client = await _listener.AcceptTcpClientAsync(cancellationToken).ConfigureAwait(false);
                using var stream = client.GetStream();

                var buffer = new byte[8192];
                var received = new MemoryStream();
                int contentLength = 0;
                bool headersParsed = false;
                var headerBuilder = new StringBuilder();

                while (!cancellationToken.IsCancellationRequested)
                {
                    var read = await stream.ReadAsync(buffer, cancellationToken).ConfigureAwait(false);
                    if (read <= 0)
                    {
                        break;
                    }
                    received.Write(buffer, 0, read);

                    if (!headersParsed)
                    {
                        var raw = Encoding.ASCII.GetString(received.ToArray());
                        var headerEnd = raw.IndexOf("\r\n\r\n", StringComparison.Ordinal);
                        if (headerEnd >= 0)
                        {
                            headerBuilder.Append(raw[..headerEnd]);
                            headersParsed = true;
                            var lines = headerBuilder.ToString().Split("\r\n");
                            var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                            for (int i = 1; i < lines.Length; i++)
                            {
                                var idx = lines[i].IndexOf(':');
                                if (idx <= 0)
                                {
                                    continue;
                                }
                                headers[lines[i][..idx].Trim()] = lines[i][(idx + 1)..].Trim();
                            }
                            if (headers.TryGetValue("Content-Length", out var lenStr) &&
                                int.TryParse(lenStr, out var len))
                            {
                                contentLength = len;
                            }

                            // Store partial body already read past the header terminator.
                            var bodyStart = headerEnd + 4;
                            var bodyBytes = received.ToArray()[bodyStart..];
                            received.SetLength(0);
                            received.Write(bodyBytes, 0, bodyBytes.Length);

                            if (received.Length >= contentLength)
                            {
                                CompleteRequest(lines[0], headers, received.ToArray()[..contentLength], stream);
                                return;
                            }
                        }
                    }
                    else if (received.Length >= contentLength)
                    {
                        var lines = headerBuilder.ToString().Split("\r\n");
                        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                        for (int i = 1; i < lines.Length; i++)
                        {
                            var idx = lines[i].IndexOf(':');
                            if (idx <= 0)
                            {
                                continue;
                            }
                            headers[lines[i][..idx].Trim()] = lines[i][(idx + 1)..].Trim();
                        }
                        CompleteRequest(lines[0], headers, received.ToArray()[..contentLength], stream);
                        return;
                    }
                }
            }
            catch (Exception ex)
            {
                _tcs.TrySetException(ex);
            }
        }

        private void CompleteRequest(
            string requestLine,
            IDictionary<string, string> headers,
            byte[] body,
            NetworkStream stream)
        {
            _tcs.TrySetResult(new StubHttpRequest(requestLine, headers, Encoding.UTF8.GetString(body)));
            try
            {
                var response = "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n";
                var bytes = Encoding.ASCII.GetBytes(response);
                stream.Write(bytes, 0, bytes.Length);
            }
            catch
            {
                // best-effort
            }
        }

        public void Dispose()
        {
            _cts.Cancel();
            try
            {
                _listener.Stop();
            }
            catch
            {
                // ignored
            }
        }
    }

    private sealed record StubHttpRequest(
        string RequestLine,
        IDictionary<string, string> Headers,
        string Body);
}
