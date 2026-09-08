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

using OpenSandbox.Config;
using OpenSandbox.Core;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace OpenSandbox;

/// <summary>
/// Provides the HTTP clients used by a sandbox SDK instance.
/// </summary>
public sealed class HttpClientProvider : IDisposable
{
    private bool _disposed;
    private readonly ILogger _logger;

    internal HttpClientProvider(ConnectionConfig connectionConfig, ILoggerFactory loggerFactory)
    {
        _logger = (loggerFactory ?? NullLoggerFactory.Instance).CreateLogger("OpenSandbox.HttpClientProvider");
        _logger.LogDebug("Creating HTTP clients for SDK instance");
        HttpClient = connectionConfig.CreateHttpClient();
        SseHttpClient = connectionConfig.CreateSseHttpClient();

        // These clients are shared by lifecycle and data-plane adapters. Keep
        // tenant credentials request-scoped so direct execd/egress calls cannot
        // inherit them through DefaultRequestHeaders.
        HttpClient.DefaultRequestHeaders.Remove(Constants.ApiKeyHeader);
        SseHttpClient.DefaultRequestHeaders.Remove(Constants.ApiKeyHeader);
    }

    /// <summary>
    /// Gets the HTTP client used for non-streaming requests.
    /// </summary>
    public HttpClient HttpClient { get; }

    /// <summary>
    /// Gets the HTTP client used for streaming requests.
    /// </summary>
    public HttpClient SseHttpClient { get; }

    /// <summary>
    /// Releases HTTP client resources.
    /// </summary>
    public void Dispose()
    {
        if (_disposed)
        {
            return;
        }

        _disposed = true;
        _logger.LogDebug("Disposing HTTP clients for SDK instance");
        HttpClient.Dispose();
        SseHttpClient.Dispose();
    }
}
