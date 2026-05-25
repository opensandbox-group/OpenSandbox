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

namespace OpenSandbox.Core;

/// <summary>
/// Default constants used throughout the OpenSandbox SDK.
/// </summary>
public static class Constants
{
    /// <summary>
    /// Default port for the execd service.
    /// </summary>
    public const int DefaultExecdPort = 44772;

    /// <summary>
    /// Default port for the egress sidecar service.
    /// </summary>
    public const int DefaultEgressPort = 18080;

    /// <summary>
    /// Default entrypoint command for sandbox containers.
    /// </summary>
    public static readonly string[] DefaultEntrypoint = new[] { "tail", "-f", "/dev/null" };

    /// <summary>
    /// Default resource limits for sandbox containers.
    /// </summary>
    public static readonly IReadOnlyDictionary<string, string> DefaultResourceLimits = new Dictionary<string, string>
    {
        ["cpu"] = "1",
        ["memory"] = "2Gi"
    };

    /// <summary>
    /// Default sandbox timeout in seconds (10 minutes).
    /// </summary>
    public const int DefaultTimeoutSeconds = 600;

    /// <summary>
    /// Default timeout for waiting until sandbox is ready in seconds.
    /// </summary>
    public const int DefaultReadyTimeoutSeconds = 30;

    /// <summary>
    /// Default polling interval for health checks in milliseconds.
    /// </summary>
    public const int DefaultHealthCheckPollingIntervalMillis = 200;

    /// <summary>
    /// Default HTTP request timeout in seconds.
    /// </summary>
    public const int DefaultRequestTimeoutSeconds = 30;

    /// <summary>
    /// Default user agent string for SDK HTTP requests.
    /// </summary>
    public const string DefaultUserAgent = "OpenSandbox-CSharp-SDK/0.1.2";

    /// <summary>
    /// Environment variable name for the OpenSandbox domain.
    /// </summary>
    public const string EnvDomain = "OPEN_SANDBOX_DOMAIN";

    /// <summary>
    /// Environment variable name for the OpenSandbox API key.
    /// </summary>
    public const string EnvApiKey = "OPEN_SANDBOX_API_KEY";

    /// <summary>
    /// Header name for the API key.
    /// </summary>
    public const string ApiKeyHeader = "OPEN-SANDBOX-API-KEY";

    /// <summary>
    /// Header name for request ID.
    /// </summary>
    public const string RequestIdHeader = "x-request-id";
}
