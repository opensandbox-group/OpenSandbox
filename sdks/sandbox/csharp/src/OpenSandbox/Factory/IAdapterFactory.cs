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
using OpenSandbox.Services;
using OpenSandbox;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.Factory;

/// <summary>
/// Options for creating a lifecycle service stack.
/// </summary>
public class CreateLifecycleStackOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public required ConnectionConfig ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets the lifecycle API base URL.
    /// </summary>
    public required string LifecycleBaseUrl { get; set; }

    /// <summary>
    /// Gets or sets the HTTP client provider for this SDK instance.
    /// </summary>
    public required HttpClientProvider HttpClientProvider { get; set; }

    /// <summary>
    /// Gets or sets the logger factory for this SDK instance.
    /// </summary>
    public required ILoggerFactory LoggerFactory { get; set; }
}

/// <summary>
/// Options for creating an execd service stack.
/// </summary>
public class CreateExecdStackOptions
{
    /// <summary>
    /// Gets or sets the connection configuration.
    /// </summary>
    public required ConnectionConfig ConnectionConfig { get; set; }

    /// <summary>
    /// Gets or sets the execd API base URL.
    /// </summary>
    public required string ExecdBaseUrl { get; set; }

    /// <summary>
    /// Gets or sets headers to apply to execd requests.
    /// If null, <see cref="ConnectionConfig.Headers"/> is used.
    /// </summary>
    public IReadOnlyDictionary<string, string>? ExecdHeaders { get; set; }

    /// <summary>
    /// Gets or sets the HTTP client provider for this SDK instance.
    /// </summary>
    public required HttpClientProvider HttpClientProvider { get; set; }

    /// <summary>
    /// Gets or sets the logger factory for this SDK instance.
    /// </summary>
    public required ILoggerFactory LoggerFactory { get; set; }
}

/// <summary>
/// Stack of lifecycle services.
/// </summary>
public class LifecycleStack
{
    /// <summary>
    /// Gets the sandboxes service.
    /// </summary>
    public required ISandboxes Sandboxes { get; init; }
}

/// <summary>
/// Stack of execd services.
/// </summary>
public class ExecdStack
{
    /// <summary>
    /// Gets the commands service.
    /// </summary>
    public required IExecdCommands Commands { get; init; }

    /// <summary>
    /// Gets the files service.
    /// </summary>
    public required ISandboxFiles Files { get; init; }

    /// <summary>
    /// Gets the health service.
    /// </summary>
    public required IExecdHealth Health { get; init; }

    /// <summary>
    /// Gets the metrics service.
    /// </summary>
    public required IExecdMetrics Metrics { get; init; }
}

public class CreateEgressStackOptions
{
    public required ConnectionConfig ConnectionConfig { get; set; }

    public required string EgressBaseUrl { get; set; }

    public IReadOnlyDictionary<string, string>? EgressHeaders { get; set; }

    public required HttpClientProvider HttpClientProvider { get; set; }

    public required ILoggerFactory LoggerFactory { get; set; }
}

public class EgressStack
{
    public required IEgress Egress { get; init; }
}

/// <summary>
/// Factory interface for creating service adapters.
/// </summary>
public interface IAdapterFactory
{
    /// <summary>
    /// Creates a lifecycle service stack.
    /// </summary>
    /// <param name="options">The creation options.</param>
    /// <returns>The lifecycle stack.</returns>
    LifecycleStack CreateLifecycleStack(CreateLifecycleStackOptions options);

    /// <summary>
    /// Creates an execd service stack.
    /// </summary>
    /// <param name="options">The creation options.</param>
    /// <returns>The execd stack.</returns>
    ExecdStack CreateExecdStack(CreateExecdStackOptions options);

    /// <summary>
    /// Creates an egress service stack.
    /// </summary>
    /// <param name="options">The creation options.</param>
    /// <returns>The egress stack.</returns>
    EgressStack CreateEgressStack(CreateEgressStackOptions options);
}
