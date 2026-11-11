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

using System.Diagnostics;
using System.Globalization;
using System.Text;
using System.Text.Json;
using OpenSandbox.Config;
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
    private readonly HttpClient? _transferHttpClient;
    private readonly TimeSpan? _transferHeaderTimeout;
    private readonly string _baseUrl;
    private readonly IReadOnlyDictionary<string, string> _defaultHeaders;
    private readonly ILogger _logger;
    private readonly WakeRetryOptions _wakeRetry;

    /// <summary>
    /// The gateway emits <c>Retry-After: 1</c> on wake-timeout responses; used
    /// as the fallback wait when a marked response arrives without the header.
    /// </summary>
    private static readonly TimeSpan WakeDefaultRetryAfter = TimeSpan.FromSeconds(1);

    /// <summary>
    /// Fixed operational ceiling for server-supplied <c>Retry-After</c> waits.
    /// Not configurable: ignoring a server's back-pressure signal is never a
    /// safe default, and the 60s guard only defends against a pathological
    /// header.
    /// </summary>
    private static readonly TimeSpan RetryAfterCap = TimeSpan.FromSeconds(60);

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
        ILogger? logger = null,
        HttpClient? transferHttpClient = null,
        TimeSpan? transferHeaderTimeout = null,
        WakeRetryOptions? wakeRetry = null)
    {
        _httpClient = httpClient ?? throw new ArgumentNullException(nameof(httpClient));
        _transferHttpClient = transferHttpClient;
        _transferHeaderTimeout = transferHeaderTimeout;
        _baseUrl = baseUrl?.TrimEnd('/') ?? throw new ArgumentNullException(nameof(baseUrl));
        _defaultHeaders = defaultHeaders ?? new Dictionary<string, string>();
        _logger = logger ?? NullLogger.Instance;
        _wakeRetry =
            wakeRetry ??
            new WakeRetryOptions(
                enabled: true,
                maxAttempts: Constants.DefaultWakeMaxAttempts,
                overallDeadline: TimeSpan.FromSeconds(Constants.DefaultWakeOverallDeadlineSeconds));
    }

    public string BaseUrl => _baseUrl;

    public HttpClientWrapper WithBaseUrl(string baseUrl) =>
        new(_httpClient, baseUrl, _defaultHeaders, _logger, _transferHttpClient, _transferHeaderTimeout, _wakeRetry);

    public async Task<T> GetAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Get, url);
                ApplyDefaultHeaders(request);
                return request;
            },
            cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Gets a JSON resource and also returns response headers, e.g. to read
    /// metadata such as the OPEN-SANDBOX-ORIGIN header.
    /// </summary>
    public async Task<(T Body, IReadOnlyDictionary<string, string> Headers)> GetWithHeadersAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Get, url);
                ApplyDefaultHeaders(request);
                return request;
            },
            cancellationToken).ConfigureAwait(false);
        var headers = response.Headers.ToDictionary(h => h.Key, h => h.Value.FirstOrDefault() ?? string.Empty, StringComparer.OrdinalIgnoreCase);
        var body = await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
        return (body, headers);
    }

    public async Task GetAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP GET {Url}", url);
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Get, url);
                ApplyDefaultHeaders(request);
                return request;
            },
            cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PostAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var response = await SendWithWakeRetryAsync(() => BuildJsonRequest(HttpMethod.Post, url, body), cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PostAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP POST {Url}", url);
        using var response = await SendWithWakeRetryAsync(() => BuildJsonRequest(HttpMethod.Post, url, body), cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> PatchAsync<T>(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var response = await SendWithWakeRetryAsync(() => BuildJsonRequest(PatchMethod, url, body), cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task PatchAsync(
        string path,
        object? body = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP PATCH {Url}", url);
        using var response = await SendWithWakeRetryAsync(() => BuildJsonRequest(PatchMethod, url, body), cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task<T> DeleteAsync<T>(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Delete, url);
                ApplyDefaultHeaders(request);
                return request;
            },
            cancellationToken).ConfigureAwait(false);
        return await HandleResponseAsync<T>(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(
        string path,
        Dictionary<string, string?>? queryParams = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildUrl(path, queryParams);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Delete, url);
                ApplyDefaultHeaders(request);
                return request;
            },
            cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(
        string path,
        object body,
        CancellationToken cancellationToken)
    {
        var url = BuildUrl(path);
        _logger.LogDebug("HTTP DELETE {Url}", url);
        using var response = await SendWithWakeRetryAsync(() => BuildJsonRequest(HttpMethod.Delete, url, body), cancellationToken).ConfigureAwait(false);
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
        // Buffered download: the request is trivially replayable, so it goes
        // through the wake retry like the JSON reads (streaming downloads via
        // ReadBytesStreamAsync stay single-shot) and each attempt is sent on the
        // transfer path, so the download is not capped by the connection
        // request timeout.
        using var response = await SendWithWakeRetryAsync(
            () =>
            {
                var request = new HttpRequestMessage(HttpMethod.Get, url);
                ApplyDefaultHeaders(request);
                if (headers != null)
                {
                    foreach (var header in headers)
                    {
                        request.Headers.TryAddWithoutValidation(header.Key, header.Value);
                    }
                }
                return request;
            },
            cancellationToken,
            SendTransferAsync).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await ReadContentBytesAsync(response.Content, cancellationToken).ConfigureAwait(false);
    }

    /// <summary>
    /// Sends a file transfer request (upload or download).
    /// When a transfer client is configured, the transfer is not capped by the connection request
    /// timeout, so a large file or a slow link is not aborted while data keeps flowing. A positive
    /// header timeout still bounds the wait for response headers: for a request without a body the
    /// deadline starts with the request, and for an upload it also starts with the request - which
    /// bounds the connect phase (DNS, TCP, TLS, pool wait) - but is cleared while the body is
    /// being written and re-armed once the body of the current hop has been sent (a redirect
    /// re-arms it per hop), so a server that stops responding fails the transfer instead of
    /// hanging it while a slow upload never counts its own body time. When the deadline expires
    /// the transfer fails with a <see cref="TaskCanceledException"/> carrying a
    /// <see cref="TimeoutException"/>, distinctly from a caller cancellation, which surfaces as an
    /// <see cref="OperationCanceledException"/> with the caller's token. Without a header timeout
    /// the transfer is bounded only by the caller token. The response body is read with the
    /// caller token; a caller that wants the whole transfer bounded passes a CancellationToken.
    /// </summary>
    public async Task<HttpResponseMessage> SendTransferAsync(
        HttpRequestMessage request,
        CancellationToken cancellationToken = default)
    {
        if (request is null)
        {
            throw new ArgumentNullException(nameof(request));
        }

        ApplyDefaultHeaders(request);

        var transferClient = _transferHttpClient;
        if (transferClient is null)
        {
            _logger.LogDebug("HTTP {Method} {Url} (transfer, shared client)", request.Method, request.RequestUri);
            return await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        }

        if (_transferHeaderTimeout is not { } headerTimeout || headerTimeout <= TimeSpan.Zero)
        {
            // A configured transfer client never falls back to the shared client's whole-call
            // request timeout; without a positive header timeout there is no deadline at all and
            // the caller token is the only bound.
            _logger.LogDebug("HTTP {Method} {Url} (transfer, no header timeout)", request.Method, request.RequestUri);
            return await transferClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken).ConfigureAwait(false);
        }

        var headerDeadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        // Armed before the send for uploads too: until the first body byte is serialized it bounds
        // the connect phase; the body signals below clear it again while the body is written.
        headerDeadline.CancelAfter(headerTimeout);
        if (request.Content is not null)
        {
            // The header deadline stays off while the body is being written - a large file or a
            // slow link must not count its own upload time against it - and is cleared again
            // before each hop of a redirect re-serializes the body.
            request.Content = new BodySentSignalingContent(
                request.Content,
                onBodySendStarted: () => DisarmHeaderDeadline(headerDeadline),
                onBodySent: () => ArmHeaderDeadline(headerDeadline, headerTimeout));
        }

        try
        {
            _logger.LogDebug("HTTP {Method} {Url} (transfer)", request.Method, request.RequestUri);
            return await transferClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, headerDeadline.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested && headerDeadline.IsCancellationRequested)
        {
            // The header deadline, not the caller, ended the send. Keep the two apart the way
            // HttpClient.Timeout does on modern runtimes: a TaskCanceledException with a
            // TimeoutException inside.
            var message = FormattableString.Invariant(
                $"The transfer request did not receive response headers within {headerTimeout}.");
            throw new TaskCanceledException(message, new TimeoutException(message));
        }
        finally
        {
            // Response headers have arrived (or the send failed): stop the header deadline and
            // release the linked registration. Body reads use the caller token, so they stay
            // cancellable and are never bounded by the header deadline. A duplex transport can
            // still be writing the request body here; disposing a source only releases its timer
            // and registration - it never cancels - and the deadline is already disarmed while a
            // body write is in flight, so this cleanup cannot cut that write short.
            headerDeadline.Dispose();
        }
    }

    private static void ArmHeaderDeadline(CancellationTokenSource headerDeadline, TimeSpan headerTimeout)
    {
        try
        {
            headerDeadline.CancelAfter(headerTimeout);
        }
        catch (ObjectDisposedException)
        {
            // The send has already returned and released the deadline: arming it is moot.
        }
    }

    private static void DisarmHeaderDeadline(CancellationTokenSource headerDeadline)
    {
        try
        {
            headerDeadline.CancelAfter(Timeout.InfiniteTimeSpan);
        }
        catch (ObjectDisposedException)
        {
            // The send has already returned and released the deadline.
        }
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

    internal static async Task<byte[]> ReadContentBytesAsync(HttpContent content, CancellationToken cancellationToken)
    {
        // Stream acquisition can buffer custom HttpContent before any cancellable read.
        using var registration = cancellationToken.Register(content.Dispose);
        try
        {
            using var source = await content.ReadAsStreamAsync().ConfigureAwait(false);
            cancellationToken.ThrowIfCancellationRequested();
            // Preallocate from a declared Content-Length so a large download does not pay the
            // doubling growth (and the transient copies it produces) of an unbounded MemoryStream;
            // an unknown or out-of-range length keeps the default growth.
            var declaredLength = content.Headers.ContentLength;
            using var buffer = new MemoryStream(
                declaredLength is > 0 and <= int.MaxValue ? (int)declaredLength.Value : 0);
            var chunk = new byte[81920];
            int read;
            while ((read = await source.ReadAsync(chunk, 0, chunk.Length, cancellationToken).ConfigureAwait(false)) > 0)
            {
                buffer.Write(chunk, 0, read);
            }

            return buffer.ToArray();
        }
        catch (Exception) when (cancellationToken.IsCancellationRequested)
        {
            throw new OperationCanceledException(cancellationToken);
        }
    }

    /// <summary>
    /// Reads a response body as a string with the caller token. Transfer responses are sent with
    /// <see cref="HttpCompletionOption.ResponseHeadersRead"/>, so their bodies are not buffered
    /// by the transport and a stalled body must stay cancellable instead of blocking the caller.
    /// The bytes are read in chunks with the token and decoded the way
    /// <c>HttpContent.ReadAsStringAsync</c> does it: a declared charset is honored with its own
    /// byte order mark skipped when present, and without one a byte order mark selects
    /// UTF-8/UTF-16/UTF-32 and is skipped. An unknown charset decodes as UTF-8 instead of
    /// failing the read. One code path on every target framework, instead of a netstandard-only
    /// fallback that CI could not reach.
    /// </summary>
    internal static async Task<string> ReadContentStringAsync(HttpContent content, CancellationToken cancellationToken)
    {
        var bytes = await ReadContentBytesAsync(content, cancellationToken).ConfigureAwait(false);
        var charset = content.Headers.ContentType?.CharSet;
        if (!string.IsNullOrEmpty(charset))
        {
            try
            {
                var encoding = Encoding.GetEncoding(charset!.Trim('"'));
                // HttpContent.ReadAsStringAsync drops the declared encoding's own preamble (byte
                // order mark) when the bytes start with it; a foreign byte order mark is kept.
                var preamble = encoding.GetPreamble();
                if (preamble.Length > 0 && bytes.Length >= preamble.Length && PreambleMatches(bytes, preamble))
                {
                    return encoding.GetString(bytes, preamble.Length, bytes.Length - preamble.Length);
                }

                return encoding.GetString(bytes);
            }
            catch (ArgumentException)
            {
                // Unknown charset: decode as UTF-8 rather than fail the read the way
                // HttpContent.ReadAsStringAsync does; an error body must stay readable.
            }
        }

        // Mirror HttpContent.ReadAsStringAsync: without a declared charset a byte order mark
        // picks the encoding and is not part of the text.
        if (bytes.Length >= 3 && bytes[0] == 0xEF && bytes[1] == 0xBB && bytes[2] == 0xBF)
        {
            return Encoding.UTF8.GetString(bytes, 3, bytes.Length - 3);
        }

        if (bytes.Length >= 4 && bytes[0] == 0xFF && bytes[1] == 0xFE && bytes[2] == 0x00 && bytes[3] == 0x00)
        {
            return new UTF32Encoding(bigEndian: false, byteOrderMark: false).GetString(bytes, 4, bytes.Length - 4);
        }

        if (bytes.Length >= 4 && bytes[0] == 0x00 && bytes[1] == 0x00 && bytes[2] == 0xFE && bytes[3] == 0xFF)
        {
            return new UTF32Encoding(bigEndian: true, byteOrderMark: false).GetString(bytes, 4, bytes.Length - 4);
        }

        if (bytes.Length >= 2 && bytes[0] == 0xFF && bytes[1] == 0xFE)
        {
            return Encoding.Unicode.GetString(bytes, 2, bytes.Length - 2);
        }

        if (bytes.Length >= 2 && bytes[0] == 0xFE && bytes[1] == 0xFF)
        {
            return Encoding.BigEndianUnicode.GetString(bytes, 2, bytes.Length - 2);
        }

        return Encoding.UTF8.GetString(bytes);
    }

    private static bool PreambleMatches(byte[] bytes, byte[] preamble)
    {
        for (var i = 0; i < preamble.Length; i++)
        {
            if (bytes[i] != preamble[i])
            {
                return false;
            }
        }

        return true;
    }

    private HttpRequestMessage BuildJsonRequest(HttpMethod method, string url, object? body)
    {
        var request = new HttpRequestMessage(method, url);
        ApplyDefaultHeaders(request);
        if (body != null)
        {
            var json = JsonSerializer.Serialize(body, JsonOptions);
            request.Content = new StringContent(json, Encoding.UTF8, "application/json");
        }
        return request;
    }

    /// <summary>
    /// Whether the response is the OSEP-0024 wake-timeout answer.
    /// <para>
    /// Recognition is header-only on a 503: the gateway strips
    /// <c>OpenSandbox-Wake</c> from every upstream response, so a matching
    /// marker proves the gateway parked the request and never forwarded it.
    /// The JSON <c>code</c> in the body is documentation for humans, not a
    /// recognition signal — a forged body without the header follows the
    /// SDK's existing (bare 503) rules untouched.
    /// </para>
    /// </summary>
    private static bool IsWakeTimeoutResponse(HttpResponseMessage response)
    {
        return (int)response.StatusCode == 503 &&
               response.Headers.TryGetValues(Constants.WakeMarkerHeader, out var values) &&
               string.Equals(values.FirstOrDefault(), Constants.WakeMarkerExpired, StringComparison.Ordinal);
    }

    /// <summary>
    /// Effective wake retry wait for a marked response: the parsed
    /// <c>Retry-After</c> (delta-seconds or HTTP-date) capped at the
    /// operational ceiling, or the 1-second fallback when the header is
    /// missing or unparseable.
    /// </summary>
    private static TimeSpan ResolveWakeRetryAfter(HttpResponseMessage response)
    {
        TimeSpan? parsed = null;
        if (response.Headers.TryGetValues("Retry-After", out var values))
        {
            var raw = values.FirstOrDefault();
            if (!string.IsNullOrWhiteSpace(raw))
            {
                var trimmed = raw.Trim();
                if (int.TryParse(trimmed, out var seconds))
                {
                    parsed = seconds <= 0 ? TimeSpan.Zero : TimeSpan.FromSeconds(seconds);
                }
                else if (
                    // HTTP dates are fixed-format RFC1123; parse with the
                    // invariant culture (with a loose invariant fallback) so
                    // hosts running a non-Gregorian culture still parse them.
                    DateTimeOffset.TryParseExact(
                        trimmed,
                        "R",
                        CultureInfo.InvariantCulture,
                        DateTimeStyles.None,
                        out var exact) ||
                    DateTimeOffset.TryParse(
                        trimmed,
                        CultureInfo.InvariantCulture,
                        DateTimeStyles.None,
                        out exact))
                {
                    var delta = exact - DateTimeOffset.UtcNow;
                    parsed = delta < TimeSpan.Zero ? TimeSpan.Zero : delta;
                }
            }
        }

        var wait = parsed ?? WakeDefaultRetryAfter;
        return wait > RetryAfterCap ? RetryAfterCap : wait;
    }

    /// <summary>
    /// Sends a request, applying the OSEP-0024 wake retry contract.
    /// <para>
    /// On a marked wake-timeout response the original request is rebuilt and
    /// resent after waiting per <c>Retry-After</c> (JSON bodies are
    /// re-serializable and the download requests are replayable, so replay is
    /// always safe), until the sandbox is <c>Running</c> or the policy budget is exhausted;
    /// exhaustion throws <see cref="SandboxWakeTimeoutException"/> while the
    /// server-side resume continues in the background. Every other response —
    /// including a bare 503 — is returned untouched. <paramref name="sender"/>
    /// selects the transport per attempt: the shared client by default, or
    /// <see cref="SendTransferAsync"/> so downloads are not capped by the
    /// connection request timeout.
    /// </para>
    /// </summary>
    internal async Task<HttpResponseMessage> SendWithWakeRetryAsync(
        Func<HttpRequestMessage> requestFactory,
        CancellationToken cancellationToken,
        Func<HttpRequestMessage, CancellationToken, Task<HttpResponseMessage>>? sender = null)
    {
        sender ??= (request, token) => _httpClient.SendAsync(request, token);
        if (!_wakeRetry.Enabled)
        {
            using var singleRequest = requestFactory();
            return await sender(singleRequest, cancellationToken).ConfigureAwait(false);
        }

        var stopwatch = Stopwatch.StartNew();
        var attempts = 0;
        while (true)
        {
            using var request = requestFactory();
            var response = await sender(request, cancellationToken).ConfigureAwait(false);
            if (!IsWakeTimeoutResponse(response))
            {
                return response;
            }

            attempts++;
            var elapsed = stopwatch.Elapsed;
            var retryAfter = ResolveWakeRetryAfter(response);
            // Budget checks bound retries, not a committed resume: the
            // server-side restore is never canceled by giving up here.
            var exhausted = attempts >= _wakeRetry.MaxAttempts ||
                            (_wakeRetry.OverallDeadline.HasValue &&
                             retryAfter > _wakeRetry.OverallDeadline.Value - elapsed);
            if (exhausted)
            {
                try
                {
                    // The transfer path sends with ResponseHeadersRead on the timeout-less transfer
                    // client, so a marked wake-timeout body is not buffered: read it through the
                    // caller-token path like every other transfer error body, or a stalled body
                    // would outlive the caller's cancellation.
                    var content = await ReadContentStringAsync(response.Content, cancellationToken).ConfigureAwait(false);
                    var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var ids)
                        ? ids.FirstOrDefault()
                        : null;
                    throw new SandboxWakeTimeoutException(
                        attempts,
                        elapsed,
                        requestId: requestId,
                        rawBody: content);
                }
                finally
                {
                    // Every other path disposes the response via using; the
                    // exhaustion throw must not leak the connection either.
                    response.Dispose();
                }
            }

            _logger.LogInformation(
                "Wake retry: marked 503 received; restore still in progress (attempt={Attempt}, retryAfter={RetryAfterMs}ms)",
                attempts,
                retryAfter.TotalMilliseconds);
            response.Dispose();
            await Task.Delay(retryAfter, cancellationToken).ConfigureAwait(false);
        }
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

    internal async Task EnsureSuccessAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (!response.IsSuccessStatusCode)
        {
            var content = await ReadContentStringAsync(response.Content, cancellationToken).ConfigureAwait(false);
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

        var message = errorMessage ?? $"Request failed with status code {(int)response.StatusCode}" +
            (string.IsNullOrEmpty(content) ? string.Empty : $": {content}");
        var sandboxErrorCode = errorCode ?? SandboxErrorCodes.UnexpectedResponse;

        throw new SandboxApiException(
            message: message,
            statusCode: (int)response.StatusCode,
            requestId: requestId,
            rawBody: rawBody,
            error: new SandboxError(sandboxErrorCode, errorMessage ?? message));
    }
}
