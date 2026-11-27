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

using FluentAssertions;
using OpenSandbox.Config;
using OpenSandbox.Core;
using Xunit;

namespace OpenSandbox.Tests;

public class ConnectionConfigTests
{
    [Fact]
    public void Constructor_WithDefaultOptions_ShouldUseDefaults()
    {
        var config = new ConnectionConfig();

        config.Protocol.Should().Be(ConnectionProtocol.Http);
        config.Domain.Should().Be("localhost:8080");
        config.ApiKey.Should().BeNull();
        config.RequestTimeoutSeconds.Should().Be(Constants.DefaultRequestTimeoutSeconds);
        config.UseServerProxy.Should().BeFalse();
        config.Headers.Should().BeEmpty();
    }

    [Fact]
    public void Constructor_WithCustomOptions_ShouldApplyOptions()
    {
        var options = new ConnectionConfigOptions
        {
            Domain = "api.example.com",
            Protocol = ConnectionProtocol.Https,
            ApiKey = "test-api-key",
            RequestTimeoutSeconds = 60,
            UseServerProxy = true,
            Headers = new Dictionary<string, string>
            {
                ["X-Custom-Header"] = "custom-value"
            }
        };

        var config = new ConnectionConfig(options);

        config.Protocol.Should().Be(ConnectionProtocol.Https);
        config.Domain.Should().Be("api.example.com");
        config.ApiKey.Should().Be("test-api-key");
        config.RequestTimeoutSeconds.Should().Be(60);
        config.UseServerProxy.Should().BeTrue();
        config.Headers.Should().ContainKey("X-Custom-Header");
        config.Headers["X-Custom-Header"].Should().Be("custom-value");
    }

    [Fact]
    public void Constructor_WithApiKey_ShouldAddApiKeyHeader()
    {
        var options = new ConnectionConfigOptions
        {
            ApiKey = "my-secret-key"
        };

        var config = new ConnectionConfig(options);

        config.Headers.Should().ContainKey(Constants.ApiKeyHeader);
        config.Headers[Constants.ApiKeyHeader].Should().Be("my-secret-key");
    }

    [Fact]
    public void GetBaseUrl_WithHttpProtocol_ShouldReturnHttpUrl()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "localhost:8080",
            Protocol = ConnectionProtocol.Http
        });

        var baseUrl = config.GetBaseUrl();

        baseUrl.Should().Be("http://localhost:8080/v1");
    }

    [Fact]
    public void GetBaseUrl_WithHttpsProtocol_ShouldReturnHttpsUrl()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "api.example.com",
            Protocol = ConnectionProtocol.Https
        });

        var baseUrl = config.GetBaseUrl();

        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithFullUrl_ShouldPreserveScheme()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "https://api.example.com"
        });

        var baseUrl = config.GetBaseUrl();

        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithV1Suffix_ShouldNotDuplicate()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "https://api.example.com/v1"
        });

        var baseUrl = config.GetBaseUrl();

        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithTrailingSlash_ShouldNormalize()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "api.example.com/"
        });

        var baseUrl = config.GetBaseUrl();

        baseUrl.Should().Be("http://api.example.com/v1");
    }

    [Fact]
    public void CreateHttpClient_ShouldReturnConfiguredClient()
    {
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            RequestTimeoutSeconds = 45
        });

        var client = config.CreateHttpClient();

        client.Should().NotBeNull();
        client.Timeout.Should().Be(TimeSpan.FromSeconds(45));
    }

    [Fact]
    public void GetHttpClient_ShouldReturnSameInstance()
    {
        var config = new ConnectionConfig();

        var client1 = config.GetHttpClient();
        var client2 = config.GetHttpClient();

        client1.Should().BeSameAs(client2);
    }

    [Fact]
    public void CreateSseHttpClient_ShouldHaveInfiniteTimeout()
    {
        var config = new ConnectionConfig();

        var client = config.CreateSseHttpClient();

        client.Should().NotBeNull();
        client.Timeout.Should().Be(Timeout.InfiniteTimeSpan);
    }

    [Fact]
    public void Constructor_WithExplicitApiKeyHeaderInAnyCase_ShouldSuppressDefaultKey()
    {
        var options = new ConnectionConfigOptions
        {
            ApiKey = "configured-key",
            Headers = new Dictionary<string, string>
            {
                ["open-sandbox-api-key"] = "explicit-key"
            }
        };

        var config = new ConnectionConfig(options);

        var apiKeyEntries = config.Headers
            .Where(h => string.Equals(h.Key, Constants.ApiKeyHeader, StringComparison.OrdinalIgnoreCase))
            .ToList();
        apiKeyEntries.Should().HaveCount(1, "an explicit API key header suppresses the default regardless of casing");
        apiKeyEntries[0].Should().Be(new KeyValuePair<string, string>("open-sandbox-api-key", "explicit-key"));
    }

    [Fact]
    public void Constructor_WithEmptyExplicitApiKeyHeader_ShouldPreserveIt()
    {
        var options = new ConnectionConfigOptions
        {
            ApiKey = "configured-key",
            Headers = new Dictionary<string, string>
            {
                ["Open-Sandbox-Api-Key"] = ""
            }
        };

        var config = new ConnectionConfig(options);

        var apiKeyEntries = config.Headers
            .Where(h => string.Equals(h.Key, Constants.ApiKeyHeader, StringComparison.OrdinalIgnoreCase))
            .ToList();
        apiKeyEntries.Should().HaveCount(1);
        apiKeyEntries[0].Value.Should().BeEmpty("a present-but-empty explicit header is not replaced by the default");
    }

    [Fact]
    public void Constructor_WithCanonicalExplicitApiKeyHeader_ShouldKeepExplicitValue()
    {
        var options = new ConnectionConfigOptions
        {
            ApiKey = "configured-key",
            Headers = new Dictionary<string, string>
            {
                [Constants.ApiKeyHeader] = "explicit-key"
            }
        };

        var config = new ConnectionConfig(options);

        config.Headers.Should().HaveCount(1);
        config.Headers[Constants.ApiKeyHeader].Should().Be("explicit-key");
    }
}
