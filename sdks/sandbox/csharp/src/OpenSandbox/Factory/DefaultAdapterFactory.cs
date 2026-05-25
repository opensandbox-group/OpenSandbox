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

using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using Microsoft.Extensions.Logging;

namespace OpenSandbox.Factory;

/// <summary>
/// Default implementation of the adapter factory.
/// </summary>
public sealed class DefaultAdapterFactory : IAdapterFactory
{
    /// <summary>
    /// Creates a new instance of the default adapter factory.
    /// </summary>
    /// <returns>A new adapter factory instance.</returns>
    public static IAdapterFactory Create() => new DefaultAdapterFactory();

    /// <inheritdoc />
    public LifecycleStack CreateLifecycleStack(CreateLifecycleStackOptions options)
    {
        var clientWrapper = new HttpClientWrapper(
            options.HttpClientProvider.HttpClient,
            options.LifecycleBaseUrl,
            options.ConnectionConfig.Headers,
            options.LoggerFactory.CreateLogger("OpenSandbox.HttpClientWrapper"));

        var sandboxes = new SandboxesAdapter(clientWrapper);

        return new LifecycleStack
        {
            Sandboxes = sandboxes
        };
    }

    /// <inheritdoc />
    public ExecdStack CreateExecdStack(CreateExecdStackOptions options)
    {
        var headers = options.ExecdHeaders ?? options.ConnectionConfig.Headers;

        var clientWrapper = new HttpClientWrapper(
            options.HttpClientProvider.HttpClient,
            options.ExecdBaseUrl,
            headers,
            options.LoggerFactory.CreateLogger("OpenSandbox.HttpClientWrapper"));

        var health = new HealthAdapter(clientWrapper);
        var metrics = new MetricsAdapter(clientWrapper);
        var files = new FilesystemAdapter(
            clientWrapper,
            options.HttpClientProvider.HttpClient,
            options.ExecdBaseUrl,
            headers);
        var commands = new CommandsAdapter(
            clientWrapper,
            options.HttpClientProvider.SseHttpClient,
            options.ExecdBaseUrl,
            headers,
            options.LoggerFactory.CreateLogger("OpenSandbox.CommandsAdapter"));

        return new ExecdStack
        {
            Commands = commands,
            Files = files,
            Health = health,
            Metrics = metrics
        };
    }

    /// <inheritdoc />
    public EgressStack CreateEgressStack(CreateEgressStackOptions options)
    {
        var headers = options.EgressHeaders ?? options.ConnectionConfig.Headers;

        var clientWrapper = new HttpClientWrapper(
            options.HttpClientProvider.HttpClient,
            options.EgressBaseUrl,
            headers,
            options.LoggerFactory.CreateLogger("OpenSandbox.HttpClientWrapper"));

        return new EgressStack
        {
            Egress = new EgressAdapter(clientWrapper)
        };
    }
}
