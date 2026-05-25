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

using System.Text;
using System.Text.Json;
using OpenSandbox.Core;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace OpenSandbox.Internal;

/// <summary>
/// Internal HTTP client wrapper for making API requests.
/// </summary>
internal sealed class HttpClientWrapper
{
    private static readonly HttpMethod PatchMethod = new("PATCH");

    private readonly HttpClient _httpClient;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _defaultHeaders;
    private readonly ILogger _logger;

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        PropertyNameCaseInsensitive = true,
        DefaultIgnoreCondition = System.Text.Json.Serialization.JsonIgnoreCondition.WhenWritingNull
    };

    public HttpClientWrapper(
        HttpClient httpClient,
        string baseUrl,
        IReadOnlyDictionary<string, string>? defaultHeaders = null,
        ILogger? logger = null)
    {
        _httpClient = httpClient ?? throw new ArgumentNullException(nameof(httpClient));
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _defaultHeaders = defaultHeaders ?? new Dictionary<string, string>();
        _logger = logger ?? NullLogger.Instance;
    }

    public string BaseUrl => _baseUrl;

    public async Task<T> GetAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task GetAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PostAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PostAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PatchAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var request = new HttpRequestMessage(PatchMethod, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PatchAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var request = new HttpRequestMessage(PatchMethod, url);
        ApplyDefaultHeaders(request);

        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> DeleteAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var request = new HttpRequestMessage(HttpMethod.Delete, url);
        ApplyDefaultHeaders(request);

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<HttpResponseMessage> SendAsync(
        HttpRequestMessage request,
        CancellationToken cancellationToken = default)
    {
        _logger.LogDebug("HTTP {Method} {Url}", request.Method, request.RequestUri);
        ApplyDefaultHeaders(request);
        return await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
    }

    public async Task<byte[]> GetBytesAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        Dictionary<string, string>? headers = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        if (headers != null)
        {
            foreach (var header in headers)
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }

        using var response = await _httpClient.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await response.Content.ReadAsByteArrayAsync().ConfigureAwait(false);
    }

    public async Task<Stream> GetStreamAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        Dictionary<string, string>? headers = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        ApplyDefaultHeaders(request);

        if (headers != null)
        {
            foreach (var header in headers)
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }

        var response = await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await response.Content.ReadAsStreamAsync().ConfigureAwait(false);
    }

    private string BuildUrl(string path, Dictionary<string, string?>? queryParams = null)
    {
        var url = path.StartsWith("/") ? $"{_baseUrl}{path}" : $"{_baseUrl}/{path}";

        if (queryParams == null || queryParams.Count == 0)
            return url;

        var queryString = string.Join("&",
            queryParams
                .Where(kv => kv.Value != null)
                .Select(kv => $"{Uri.EscapeDataString(kv.Key)}={Uri.EscapeDataString(kv.Value!)}"));

        return string.IsNullOrEmpty(queryString) ? url : $"{url}?{queryString}";
    }

    private void ApplyDefaultHeaders(HttpRequestMessage request)
    {
        foreach (var header in _defaultHeaders)
        {
            if (!request.Headers.Contains(header.Key))
            {
                request.Headers.TryAddWithoutValidation(header.Key, header.Value);
            }
        }
    }

    private async Task<T> HandleResponseAsync<T>(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);

        if (!response.IsSuccessStatusCode)
        {
            LogHttpFailure(response);
            ThrowApiException(response, content);
        }

        if (string.IsNullOrEmpty(content))
        {
            throw new SandboxApiException(
                message: "Unexpected empty response body",
                statusCode: (int)response.StatusCode,
                error: new SandboxError(SandboxErrorCodes.UnexpectedResponse, "Unexpected empty response body"),
                rawBody: content);
        }

        try
        {
            return JsonSerializer.Deserialize<T>(content, JsonOptions)!;
        }
        catch (JsonException ex)
        {
            throw new SandboxApiException(
                message: $"Failed to deserialize response: {ex.Message}",
                statusCode: (int)response.StatusCode,
                rawBody: content,
                innerException: ex);
        }
    }

    private async Task EnsureSuccessAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (!response.IsSuccessStatusCode)
        {
            var content = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            LogHttpFailure(response);
            ThrowApiException(response, content);
        }
    }

    private void LogHttpFailure(HttpResponseMessage response)
    {
        var request = response.RequestMessage;
        var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
            ? values.FirstOrDefault()
            : null;

        _logger.LogError(
            "HTTP request failed: method={Method}, url={Url}, status={StatusCode}, requestId={RequestId}",
            request?.Method.Method ?? "UNKNOWN",
            request?.RequestUri?.ToString() ?? "UNKNOWN",
            (int)response.StatusCode,
            requestId ?? string.Empty);
    }

    private static void ThrowApiException(HttpResponseMessage response, string content)
    {
        var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
            ? values.FirstOrDefault()
            : null;

        string? errorMessage = null;
        string? errorCode = null;
        object? rawBody = content;

        if (!string.IsNullOrEmpty(content))
        {
            try
            {
                var parsed = JsonSerializer.Deserialize<Dictionary<string, JsonElement>>(content, JsonOptions);
                if (parsed != null)
                {
                    rawBody = parsed;
                    if (parsed.TryGetValue("message", out var msg))
                        errorMessage = msg.GetString();
                    if (parsed.TryGetValue("error", out var err) && err.ValueKind == JsonValueKind.Object)
                    {
                        if (err.TryGetProperty("message", out var errMsg))
                            errorMessage = errorMessage ?? errMsg.GetString();
                        if (err.TryGetProperty("code", out var errCode))
                            errorCode = errCode.GetString();
                    }
                    if (parsed.TryGetValue("code", out var code))
                        errorCode = errorCode ?? code.GetString();
                }
            }
            catch
            {
                // Ignore JSON parse errors
            }
        }

        var message = errorMessage ?? $"Request failed with status code {(int)response.StatusCode}";
        var sandboxErrorCode = errorCode ?? SandboxErrorCodes.UnexpectedResponse;

        throw new SandboxApiException(
            message: message,
            statusCode: (int)response.StatusCode,
            requestId: requestId,
            rawBody: rawBody,
            error: new SandboxError(sandboxErrorCode, errorMessage ?? message));
    }
}
