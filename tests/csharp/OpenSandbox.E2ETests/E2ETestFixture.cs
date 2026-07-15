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
using Xunit;

namespace OpenSandbox.E2ETests;

/// <summary>
/// Shared fixture for E2E tests providing common configuration.
/// </summary>
public sealed class E2ETestFixture : IAsyncLifetime
{
    public string DefaultImage { get; }

    public ConnectionConfig ConnectionConfig { get; }

    public ConnectionConfig ServerProxyConnectionConfig { get; }

    public int DefaultTimeoutSeconds { get; } = 1200;

    public int DefaultReadyTimeoutSeconds { get; } = 90;

    public E2ETestFixture()
    {
        DefaultImage =
            Environment.GetEnvironmentVariable("OPENSANDBOX_SANDBOX_DEFAULT_IMAGE")
            ?? Environment.GetEnvironmentVariable("SANDBOX_IMAGE")
            ?? "opensandbox/code-interpreter:latest";

        var domain =
            Environment.GetEnvironmentVariable("OPENSANDBOX_TEST_DOMAIN")
            ?? Environment.GetEnvironmentVariable("SANDBOX_DOMAIN")
            ?? "localhost:8080";

        var apiKey =
            Environment.GetEnvironmentVariable("OPENSANDBOX_TEST_API_KEY")
            ?? Environment.GetEnvironmentVariable("SANDBOX_API_KEY");

        var protocolRaw =
            Environment.GetEnvironmentVariable("OPENSANDBOX_TEST_PROTOCOL")
            ?? Environment.GetEnvironmentVariable("SANDBOX_PROTOCOL")
            ?? "http";

        var protocol = protocolRaw.Equals("https", StringComparison.OrdinalIgnoreCase)
            ? ConnectionProtocol.Https
            : ConnectionProtocol.Http;

        ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = domain,
            Protocol = protocol,
            ApiKey = apiKey,
            RequestTimeoutSeconds = 180
        });

        ServerProxyConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = domain,
            Protocol = protocol,
            ApiKey = apiKey,
            RequestTimeoutSeconds = 180,
            UseServerProxy = true
        });
    }

    public Task InitializeAsync()
    {
        return Task.CompletedTask;
    }

    public Task DisposeAsync()
    {
        return Task.CompletedTask;
    }
}

[CollectionDefinition("CSharp E2E Tests")]
public sealed class E2ETestCollection : ICollectionFixture<E2ETestFixture>
{
}
