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

using System.Net.Http.Headers;
using System.Text.Json;
using System.Text.Json.Serialization;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using OpenSandbox.Config;
using OpenSandbox.Core;

namespace OpenSandbox.Internal;

/// <summary>
/// Fire-and-forget reporter for SDK-side sandbox lifecycle metrics.
///
/// Behavior matches the other language SDKs (Go/JS/Python):
/// posts a small JSON body to <c>{baseUrl}/metrics/events</c> and never
/// blocks or surfaces errors to the caller. Honors the connection-level
/// <see cref="ConnectionConfig.DisableMetrics"/> opt-out and the
/// <c>OPENSANDBOX_DISABLE_METRICS=1</c> environment variable.
/// </summary>
internal static class LifecycleMetricsReporter
{
    private static readonly JsonSerializerOptions SerializerOptions = new()
    {
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
        Encoder = System.Text.Encodings.Web.JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
    };

    /// <summary>
    /// Reports a sandbox.create latency event. Never throws.
    /// </summary>
    /// <param name="connectionConfig">Connection configuration used to derive URL, headers, and opt-out.</param>
    /// <param name="sandboxId">Optional created sandbox ID.</param>
    /// <param name="image">Optional image or snapshot reference used for creation.</param>
    /// <param name="createDurationMs">Measured create latency in milliseconds.</param>
    /// <param name="success">Whether the create flow succeeded end-to-end.</param>
    /// <param name="loggerFactory">Optional logger factory for debug-level failure logs.</param>
    public static void ReportSandboxCreate(
        ConnectionConfig connectionConfig,
        string? sandboxId,
        string? image,
        long createDurationMs,
        bool success,
        ILoggerFactory? loggerFactory = null)
    {
        if (connectionConfig.DisableMetrics)
        {
            return;
        }

        var logger = (loggerFactory ?? NullLoggerFactory.Instance)
            .CreateLogger("OpenSandbox.LifecycleMetricsReporter");

        // Telemetry is best-effort and MUST NOT surface any exception to the
        // caller. In particular, this method is invoked from Sandbox.CreateAsync's
        // failure path; any exception thrown while building the payload/URL would
        // otherwise replace the original create failure. Guard the whole method
        // body — including construction — with a top-level try/catch.
        byte[] body;
        string url;
        try
        {
            var payload = BuildPayload(sandboxId, image, createDurationMs, success);
            body = JsonSerializer.SerializeToUtf8Bytes(payload, SerializerOptions);
            url = $"{connectionConfig.GetBaseUrl().TrimEnd('/')}/metrics/events";
        }
        catch (Exception ex)
        {
            logger.LogDebug(ex, "Failed to build sandbox.create metrics request");
            return;
        }

        // Fire-and-forget: swallow any exception. Never block the caller.
        _ = Task.Run(async () =>
        {
            try
            {
                using var client = new HttpClient
                {
                    Timeout = TimeSpan.FromSeconds(Math.Max(1, connectionConfig.RequestTimeoutSeconds)),
                };
                using var content = new ByteArrayContent(body);
                content.Headers.ContentType = new MediaTypeHeaderValue("application/json");

                using var request = new HttpRequestMessage(HttpMethod.Post, url) { Content = content };
                request.Headers.UserAgent.Clear();
                request.Headers.UserAgent.ParseAdd(connectionConfig.UserAgent);
                foreach (var header in connectionConfig.Headers)
                {
                    // Skip content headers here; content-type is set above.
                    if (string.Equals(header.Key, "Content-Type", StringComparison.OrdinalIgnoreCase))
                    {
                        continue;
                    }
                    request.Headers.TryAddWithoutValidation(header.Key, header.Value);
                }
                if (!string.IsNullOrEmpty(connectionConfig.ApiKey))
                {
                    request.Headers.Remove(Constants.ApiKeyHeader);
                    request.Headers.TryAddWithoutValidation(Constants.ApiKeyHeader, connectionConfig.ApiKey);
                }

                using var response = await client.SendAsync(request).ConfigureAwait(false);
                // Drain content to allow connection reuse; ignore status.
                _ = await response.Content.ReadAsByteArrayAsync().ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                logger.LogDebug(ex, "Failed to report sandbox.create metrics");
            }
        });
    }

    internal static IReadOnlyDictionary<string, object> BuildPayload(
        string? sandboxId,
        string? image,
        long createDurationMs,
        bool success)
    {
        var payload = new Dictionary<string, object>
        {
            ["eventType"] = "sandbox.create",
            ["createDurationMs"] = Math.Max(0, createDurationMs),
            ["success"] = success,
        };
        if (!string.IsNullOrEmpty(sandboxId))
        {
            payload["sandboxId"] = sandboxId!;
        }
        if (!string.IsNullOrEmpty(image))
        {
            payload["image"] = image!;
        }
        return payload;
    }
}
