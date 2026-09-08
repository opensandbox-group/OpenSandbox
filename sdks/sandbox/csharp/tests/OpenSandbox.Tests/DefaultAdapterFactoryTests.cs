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

using FluentAssertions;
using Microsoft.Extensions.Logging.Abstractions;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Factory;
using Xunit;

namespace OpenSandbox.Tests;

public class DefaultAdapterFactoryTests
{
    [Fact]
    public void BuildDataPlaneHeaders_InDirectMode_ShouldRemoveApiKeyCaseInsensitively()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            ApiKey = "tenant-secret",
            Headers = new Dictionary<string, string>
            {
                ["open-sandbox-api-key"] = "explicit-secret",
                ["X-Custom-Header"] = "custom-value"
            },
            UseServerProxy = false
        });

        var headers = DefaultAdapterFactory.BuildDataPlaneHeaders(
            config,
            new Dictionary<string, string> { ["X-Endpoint-Token"] = "route-token" });

        headers.Keys.Should().NotContain(key =>
            key.Equals(Constants.ApiKeyHeader, StringComparison.OrdinalIgnoreCase));
        headers["X-Custom-Header"].Should().Be("custom-value");
        headers["X-Endpoint-Token"].Should().Be("route-token");
    }

    [Fact]
    public void BuildDataPlaneHeaders_InServerProxyMode_ShouldRetainApiKey()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            ApiKey = "tenant-secret",
            UseServerProxy = true
        });

        var headers = DefaultAdapterFactory.BuildDataPlaneHeaders(config, null);

        headers[Constants.ApiKeyHeader].Should().Be("tenant-secret");
    }

    [Fact]
    public void BuildDataPlaneHeaders_InServerProxyMode_ShouldPreferEndpointApiKey()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            ApiKey = "tenant-secret",
            UseServerProxy = true
        });

        var headers = DefaultAdapterFactory.BuildDataPlaneHeaders(
            config,
            new Dictionary<string, string>
            {
                ["open-sandbox-api-key"] = "endpoint-secret"
            });

        headers[Constants.ApiKeyHeader].Should().Be("endpoint-secret");
    }

    [Fact]
    public void SharedHttpClients_ShouldNotCarryTenantApiKeyByDefault()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            ApiKey = "tenant-secret",
            UseServerProxy = false
        });

        using var provider = new HttpClientProvider(config, NullLoggerFactory.Instance);

        provider.HttpClient.DefaultRequestHeaders.Contains(Constants.ApiKeyHeader).Should().BeFalse();
        provider.SseHttpClient.DefaultRequestHeaders.Contains(Constants.ApiKeyHeader).Should().BeFalse();
        config.Headers[Constants.ApiKeyHeader].Should().Be("tenant-secret");
    }
}
