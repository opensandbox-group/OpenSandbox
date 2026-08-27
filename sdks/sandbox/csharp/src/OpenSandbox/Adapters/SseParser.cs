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

using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;
using OpenSandbox.Core;

namespace OpenSandbox.Adapters;

/// <summary>
/// Parser for Server-Sent Events (SSE) streams.
/// Supports both standard SSE frames (data: {...}) and newline-delimited JSON.
/// </summary>
internal static class SseParser
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = true
    };

    /// <summary>
    /// Parses an SSE-like stream that may be either:
    /// - standard SSE frames (data: {...}\n\n)
    /// - newline-delimited JSON (one JSON object per line)
    /// </summary>
    /// <typeparam name="T">The type to deserialize each event to.</typeparam>
    /// <param name="response">The HTTP response to parse.</param>
    /// <param name="fallbackErrorMessage">Error message to use if parsing fails.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>An async enumerable of parsed events.</returns>
    public static async IAsyncEnumerable<T> ParseJsonEventStreamAsync<T>(
        HttpResponseMessage response,
        string? fallbackErrorMessage = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        if (!response.IsSuccessStatusCode)
        {
            var text = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            var requestId = response.Headers.TryGetValues(Constants.RequestIdHeader, out var values)
                ? values.FirstOrDefault()
                : null;

            object? parsed = null;
            string? errorMessage = null;
            string? errorCode = null;

            if (!string.IsNullOrEmpty(text))
            {
                try
                {
                    parsed = JsonSerializer.Deserialize<Dictionary<string, object>>(text, JsonOptions);
                    if (parsed is Dictionary<string, object> dict)
                    {
                        if (dict.TryGetValue("message", out var msg))
                            errorMessage = msg?.ToString();
                        if (dict.TryGetValue("code", out var code))
                            errorCode = code?.ToString();
                    }
                }
                catch
                {
                    // Ignore JSON parse errors
                }
            }

            var message = errorMessage ?? $"{fallbackErrorMessage ?? $"Stream request failed (status={(int)response.StatusCode})"}" +
                (string.IsNullOrEmpty(text) ? string.Empty : $": {text}");
            var sandboxErrorCode = errorCode ?? SandboxErrorCodes.UnexpectedResponse;

            throw new SandboxApiException(
                message: message,
                statusCode: (int)response.StatusCode,
                requestId: requestId,
                rawBody: parsed ?? text,
                error: new SandboxError(sandboxErrorCode, errorMessage ?? message));
        }

        var stream = await response.Content.ReadAsStreamAsync().ConfigureAwait(false);
        using var reader = new StreamReader(stream, Encoding.UTF8);

        while (true)
        {
            cancellationToken.ThrowIfCancellationRequested();

#if NET7_0_OR_GREATER
            var line = await reader.ReadLineAsync(cancellationToken).ConfigureAwait(false);
#else
            var line = await reader.ReadLineAsync().ConfigureAwait(false);
#endif
            if (line == null)
                break;

            var trimmedLine = line.Trim();

            // Skip empty lines
            if (string.IsNullOrEmpty(trimmedLine))
                continue;

            // Skip SSE comments
            if (trimmedLine.StartsWith(":"))
                continue;

            // Skip SSE metadata lines
            if (trimmedLine.StartsWith("event:", StringComparison.OrdinalIgnoreCase) ||
                trimmedLine.StartsWith("id:", StringComparison.OrdinalIgnoreCase) ||
                trimmedLine.StartsWith("retry:", StringComparison.OrdinalIgnoreCase))
                continue;

            // Extract JSON from SSE data line or use as-is for NDJSON
            var jsonLine = trimmedLine.StartsWith("data:", StringComparison.OrdinalIgnoreCase)
                ? trimmedLine.Substring(5).Trim()
                : trimmedLine;

            if (string.IsNullOrEmpty(jsonLine))
                continue;

            var parsedEvent = TryParseJson<T>(jsonLine);
            if (parsedEvent != null)
            {
                yield return parsedEvent;
            }
        }
    }

    private static T? TryParseJson<T>(string json)
    {
        try
        {
            return JsonSerializer.Deserialize<T>(json, JsonOptions);
        }
        catch
        {
            return default;
        }
    }
}
