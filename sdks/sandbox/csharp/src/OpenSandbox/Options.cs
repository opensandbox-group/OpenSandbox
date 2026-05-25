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
using OpenSandbox.Factory;
using OpenSandbox.Models;

namespace OpenSandbox;

/// <summary>
/// Options for creating a new sandbox.
/// </summary>
public class SandboxCreateOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public ConnectionConfig? ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets diagnostics options such as logging.
    /// </summary>
    public SdkDiagnosticsOptions? Diagnostics { get; set; }

    /// <summary>
    /// Gets or sets the adapter factory for advanced customization.
    /// </summary>
    public IAdapterFactory? AdapterFactory { get; set; }

    /// <summary>
    /// Gets or sets the container image URI (e.g., "python:3.11").
    /// Can also be an ImageSpec object with authentication.
    /// </summary>
    public string? Image { get; set; }

    /// <summary>
    /// Gets or sets the image authentication credentials.
    /// </summary>
    public ImageAuth? ImageAuth { get; set; }

    /// <summary>
    /// Gets or sets the snapshot identifier to restore from.
    /// </summary>
    public string? SnapshotId { get; set; }

    /// <summary>
    /// Gets or sets the entrypoint command for the sandbox.
    /// Defaults to ["tail", "-f", "/dev/null"].
    /// </summary>
    public IReadOnlyList<string>? Entrypoint { get; set; }

    /// <summary>
    /// Gets or sets the environment variables to inject into the sandbox.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Env { get; set; }

    /// <summary>
    /// Gets or sets the custom metadata tags.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the network policy for the sandbox.
    /// </summary>
    public NetworkPolicy? NetworkPolicy { get; set; }

    /// <summary>
    /// Gets or sets an optional runtime platform constraint for sandbox provisioning.
    /// </summary>
    public PlatformSpec? Platform { get; set; }

    /// <summary>
    /// Gets or sets storage volumes mounted into the sandbox.
    /// </summary>
    public IReadOnlyList<Volume>? Volumes { get; set; }

    /// <summary>
    /// Gets or sets the extension parameters.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Extensions { get; set; }

    /// <summary>
    /// Gets or sets whether to enable secured access for sandbox endpoints.
    /// </summary>
    public bool SecureAccess { get; set; }

    /// <summary>
    /// Gets or sets the resource limits.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Resource { get; set; }

    /// <summary>
    /// Gets or sets the sandbox timeout in seconds.
    /// </summary>
    public int? TimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets whether the sandbox should disable automatic expiration and require explicit cleanup.
    /// </summary>
    public bool ManualCleanup { get; set; }

    /// <summary>
    /// Gets or sets whether to skip health checks during creation.
    /// </summary>
    public bool SkipHealthCheck { get; set; }

    /// <summary>
    /// Gets or sets a custom health check function.
    /// </summary>
    public Func<Sandbox, Task<bool>>? HealthCheck { get; set; }

    /// <summary>
    /// Gets or sets the timeout for waiting until ready in seconds.
    /// </summary>
    public int? ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the health check polling interval in milliseconds.
    /// </summary>
    public int? HealthCheckPollingInterval { get; set; }
}

/// <summary>
/// Options for connecting to an existing sandbox.
/// </summary>
public class SandboxConnectOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public ConnectionConfig? ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets diagnostics options such as logging.
    /// </summary>
    public SdkDiagnosticsOptions? Diagnostics { get; set; }

    /// <summary>
    /// Gets or sets the adapter factory for advanced customization.
    /// </summary>
    public IAdapterFactory? AdapterFactory { get; set; }

    /// <summary>
    /// Gets or sets the ID of the sandbox to connect to.
    /// </summary>
    public required string SandboxId { get; set; }

    /// <summary>
    /// Gets or sets whether to skip health checks after connecting.
    /// </summary>
    public bool SkipHealthCheck { get; set; }

    /// <summary>
    /// Gets or sets a custom health check function.
    /// </summary>
    public Func<Sandbox, Task<bool>>? HealthCheck { get; set; }

    /// <summary>
    /// Gets or sets the timeout for waiting until ready in seconds.
    /// </summary>
    public int? ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the health check polling interval in milliseconds.
    /// </summary>
    public int? HealthCheckPollingInterval { get; set; }
}

/// <summary>
/// Options for resuming a sandbox.
/// </summary>
public class SandboxResumeOptions
{
    /// <summary>
    /// Gets or sets whether to skip health checks after resuming.
    /// </summary>
    public bool SkipHealthCheck { get; set; }

    /// <summary>
    /// Gets or sets the timeout for waiting until ready in seconds.
    /// </summary>
    public int? ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the health check polling interval in milliseconds.
    /// </summary>
    public int? HealthCheckPollingInterval { get; set; }
}

/// <summary>
/// Options for waiting until a sandbox is ready.
/// </summary>
public class WaitUntilReadyOptions
{
    /// <summary>
    /// Gets or sets the timeout in seconds.
    /// </summary>
    public int ReadyTimeoutSeconds { get; set; }

    /// <summary>
    /// Gets or sets the polling interval in milliseconds.
    /// </summary>
    public int PollingIntervalMillis { get; set; }

    /// <summary>
    /// Gets or sets a custom health check function.
    /// </summary>
    public Func<Sandbox, Task<bool>>? HealthCheck { get; set; }
}

/// <summary>
/// Options for creating a sandbox manager.
/// </summary>
public class SandboxManagerOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public ConnectionConfig? ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets diagnostics options such as logging.
    /// </summary>
    public SdkDiagnosticsOptions? Diagnostics { get; set; }

    /// <summary>
    /// Gets or sets the adapter factory for advanced customization.
    /// </summary>
    public IAdapterFactory? AdapterFactory { get; set; }
}

/// <summary>
/// Filter options for listing sandboxes.
/// </summary>
public class SandboxFilter
{
    /// <summary>
    /// Gets or sets the states to filter by.
    /// </summary>
    public IReadOnlyList<string>? States { get; set; }

    /// <summary>
    /// Gets or sets the metadata to filter by.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the page number (1-indexed).
    /// </summary>
    public int? Page { get; set; }

    /// <summary>
    /// Gets or sets the page size.
    /// </summary>
    public int? PageSize { get; set; }
}
