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

using OpenSandbox.Core;

namespace OpenSandbox.Config;

/// <summary>
/// Connection protocol for the OpenSandbox API.
/// </summary>
public enum ConnectionProtocol
{
    /// <summary>
    /// HTTP protocol.
    /// </summary>
    Http,

    /// <summary>
    /// HTTPS protocol.
    /// </summary>
    Https
}

/// <summary>
/// Options for configuring a <see cref="ConnectionConfig"/>.
/// </summary>
public class ConnectionConfigOptions
{
    /// <summary>
    /// Gets or sets the API server domain (host[:port]) without scheme.
    /// Examples: "localhost:8080", "api.opensandbox.io"
    /// You may also pass a full URL (e.g. "http://localhost:8080" or "https://api.example.com").
    /// </summary>
    public string? Domain { get; set; }

    /// <summary>
    /// Gets or sets the connection protocol (http or https).
    /// </summary>
    public ConnectionProtocol? Protocol { get; set; }

    /// <summary>
    /// Gets or sets the API key for authentication.
    /// </summary>
    public string? ApiKey { get; set; }

    /// <summary>
    /// Gets or sets additional headers to include in requests.
    /// </summary>
    public Dictionary<string, string>? Headers { get; set; }

    /// <summary>
    /// Gets or sets the request timeout in seconds.
    /// Defaults to 30 seconds.
    /// </summary>
    public int? RequestTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets whether to use server-proxied endpoint URLs.
    /// </summary>
    public bool? UseServerProxy { get; set; }

    /// <summary>
    /// Gets or sets the endpoint cache TTL in seconds. Default: 600 (10 minutes).
    /// </summary>
    public int? EndpointCacheTtlSeconds { get; set; }

    /// <summary>
    /// Gets or sets the maximum number of cached endpoint entries. Default: 1024.
    /// </summary>
    public int? EndpointCacheSize { get; set; }

    /// <summary>
    /// Gets or sets whether to disable endpoint caching entirely.
    /// </summary>
    public bool? EndpointCacheDisabled { get; set; }
}

/// <summary>
/// Configuration for connecting to the OpenSandbox API.
/// </summary>
/// <remarks>
/// This type is thread-safe for concurrent reads and lazy <see cref="GetHttpClient"/> initialization.
/// The HttpClient returned by <see cref="GetHttpClient"/> is shared per <see cref="ConnectionConfig"/> instance.
/// </remarks>
public sealed class ConnectionConfig
{
    /// <summary>
    /// Gets the connection protocol.
    /// </summary>
    public ConnectionProtocol Protocol { get; }

    /// <summary>
    /// Gets the API server domain.
    /// </summary>
    public string Domain { get; }

    /// <summary>
    /// Gets the API key for authentication.
    /// </summary>
    public string? ApiKey { get; }

    /// <summary>
    /// Gets the additional headers to include in requests.
    /// </summary>
    public IReadOnlyDictionary<string, string> Headers { get; }

    /// <summary>
    /// Gets the request timeout in seconds.
    /// </summary>
    public int RequestTimeoutSeconds { get; }

    /// <summary>
    /// Gets whether server-proxied endpoint URLs should be requested.
    /// </summary>
    public bool UseServerProxy { get; }

    /// <summary>
    /// Gets the endpoint cache TTL in seconds.
    /// </summary>
    public int EndpointCacheTtlSeconds { get; }

    /// <summary>
    /// Gets the maximum number of cached endpoint entries.
    /// </summary>
    public int EndpointCacheSize { get; }

    /// <summary>
    /// Gets whether endpoint caching is disabled.
    /// </summary>
    public bool EndpointCacheDisabled { get; }

    /// <summary>
    /// Gets the user agent string.
    /// </summary>
    public string UserAgent { get; } = Constants.DefaultUserAgent;

    private HttpClient? _httpClient;
    private readonly object _httpClientLock = new();

    /// <summary>
    /// Initializes a new instance of the <see cref="ConnectionConfig"/> class.
    /// </summary>
    /// <param name="options">The configuration options.</param>
    public ConnectionConfig(ConnectionConfigOptions? options = null)
    {
        options ??= new ConnectionConfigOptions();

        var envDomain = Environment.GetEnvironmentVariable(Constants.EnvDomain);
        var envApiKey = Environment.GetEnvironmentVariable(Constants.EnvApiKey);

        var rawDomain = options.Domain ?? envDomain ?? "localhost:8080";
        var (protocol, domainBase) = NormalizeDomainBase(rawDomain);

        Protocol = protocol ?? options.Protocol ?? ConnectionProtocol.Http;
        Domain = domainBase;
        ApiKey = options.ApiKey ?? envApiKey;
        RequestTimeoutSeconds = options.RequestTimeoutSeconds ?? Constants.DefaultRequestTimeoutSeconds;
        UseServerProxy = options.UseServerProxy ?? false;
        EndpointCacheTtlSeconds = options.EndpointCacheTtlSeconds ?? 600;
        EndpointCacheSize = options.EndpointCacheSize ?? 1024;
        EndpointCacheDisabled = options.EndpointCacheDisabled ?? false;

        var headers = new Dictionary<string, string>(options.Headers ?? new Dictionary<string, string>());

        // Add API key header if not already present
        if (!string.IsNullOrEmpty(ApiKey) && !headers.ContainsKey(Constants.ApiKeyHeader))
        {
            headers[Constants.ApiKeyHeader] = ApiKey;
        }

        Headers = headers;
    }

    /// <summary>
    /// Gets the base URL for API requests.
    /// </summary>
    /// <returns>The base URL including the /v1 prefix.</returns>
    public string GetBaseUrl()
    {
        if (Domain.StartsWith("http://", StringComparison.OrdinalIgnoreCase) ||
            Domain.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
        {
            return $"{StripV1Suffix(Domain)}/v1";
        }

        var scheme = Protocol == ConnectionProtocol.Https ? "https" : "http";
        return $"{scheme}://{StripV1Suffix(Domain)}/v1";
    }

    /// <summary>
    /// Gets or creates an HttpClient configured for this connection.
    /// </summary>
    /// <returns>A configured HttpClient instance.</returns>
    public HttpClient GetHttpClient()
    {
        if (_httpClient != null)
        {
            return _httpClient;
        }

        lock (_httpClientLock)
        {
            if (_httpClient != null)
            {
                return _httpClient;
            }

            _httpClient = CreateHttpClient();
            return _httpClient;
        }
    }

    /// <summary>
    /// Creates a new HttpClient configured for this connection.
    /// </summary>
    /// <returns>A new configured HttpClient instance.</returns>
    public HttpClient CreateHttpClient()
    {
        var handler = new HttpClientHandler
        {
            AutomaticDecompression = System.Net.DecompressionMethods.GZip | System.Net.DecompressionMethods.Deflate
        };

        var client = new HttpClient(handler)
        {
            Timeout = TimeSpan.FromSeconds(RequestTimeoutSeconds)
        };

        // Set default headers
        client.DefaultRequestHeaders.UserAgent.ParseAdd(UserAgent);

        foreach (var header in Headers)
        {
            if (!client.DefaultRequestHeaders.TryAddWithoutValidation(header.Key, header.Value))
            {
                // Some headers need to be added differently
                if (header.Key.Equals("Content-Type", StringComparison.OrdinalIgnoreCase))
                {
                    continue; // Content-Type is set per request
                }
            }
        }

        return client;
    }

    /// <summary>
    /// Creates a new HttpClient configured for SSE (Server-Sent Events) streaming.
    /// This client has no timeout to allow for long-running streams.
    /// </summary>
    /// <returns>A new configured HttpClient instance for SSE.</returns>
    public HttpClient CreateSseHttpClient()
    {
        var handler = new HttpClientHandler
        {
            AutomaticDecompression = System.Net.DecompressionMethods.GZip | System.Net.DecompressionMethods.Deflate
        };

        var client = new HttpClient(handler)
        {
            Timeout = Timeout.InfiniteTimeSpan
        };

        // Set default headers
        client.DefaultRequestHeaders.UserAgent.ParseAdd(UserAgent);

        foreach (var header in Headers)
        {
            client.DefaultRequestHeaders.TryAddWithoutValidation(header.Key, header.Value);
        }

        return client;
    }

    private static (ConnectionProtocol?, string) NormalizeDomainBase(string input)
    {
        // Accept a full URL and preserve its path prefix (if any)
        if (input.StartsWith("http://", StringComparison.OrdinalIgnoreCase) ||
            input.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
        {
            var uri = new Uri(input);
            var protocol = uri.Scheme.Equals("https", StringComparison.OrdinalIgnoreCase)
                ? ConnectionProtocol.Https
                : ConnectionProtocol.Http;

            var baseUrl = $"{uri.Scheme}://{uri.Authority}{uri.AbsolutePath}";
            return (protocol, StripV1Suffix(baseUrl.TrimEnd('/')));
        }

        // No scheme: treat as "host[:port]" or "host[:port]/prefix"
        return (null, StripV1Suffix(input.TrimEnd('/')));
    }

    private static string StripV1Suffix(string s)
    {
        var trimmed = s.TrimEnd('/');
        return trimmed.EndsWith("/v1", StringComparison.OrdinalIgnoreCase)
            ? trimmed[..^3]
            : trimmed;
    }
}
