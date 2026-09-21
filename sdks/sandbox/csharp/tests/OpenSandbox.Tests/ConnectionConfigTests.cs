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
        // Arrange & Act
        var config = new ConnectionConfig();

        // Assert
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
        // Arrange
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

        // Act
        var config = new ConnectionConfig(options);

        // Assert
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
        // Arrange
        var options = new ConnectionConfigOptions
        {
            ApiKey = "my-secret-key"
        };

        // Act
        var config = new ConnectionConfig(options);

        // Assert
        config.Headers.Should().ContainKey(Constants.ApiKeyHeader);
        config.Headers[Constants.ApiKeyHeader].Should().Be("my-secret-key");
    }

    [Fact]
    public void GetBaseUrl_WithHttpProtocol_ShouldReturnHttpUrl()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "localhost:8080",
            Protocol = ConnectionProtocol.Http
        });

        // Act
        var baseUrl = config.GetBaseUrl();

        // Assert
        baseUrl.Should().Be("http://localhost:8080/v1");
    }

    [Fact]
    public void GetBaseUrl_WithHttpsProtocol_ShouldReturnHttpsUrl()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "api.example.com",
            Protocol = ConnectionProtocol.Https
        });

        // Act
        var baseUrl = config.GetBaseUrl();

        // Assert
        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithFullUrl_ShouldPreserveScheme()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "https://api.example.com"
        });

        // Act
        var baseUrl = config.GetBaseUrl();

        // Assert
        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithV1Suffix_ShouldNotDuplicate()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "https://api.example.com/v1"
        });

        // Act
        var baseUrl = config.GetBaseUrl();

        // Assert
        baseUrl.Should().Be("https://api.example.com/v1");
    }

    [Fact]
    public void GetBaseUrl_WithTrailingSlash_ShouldNormalize()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "api.example.com/"
        });

        // Act
        var baseUrl = config.GetBaseUrl();

        // Assert
        baseUrl.Should().Be("http://api.example.com/v1");
    }

    [Fact]
    public void CreateHttpClient_ShouldReturnConfiguredClient()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            RequestTimeoutSeconds = 45
        });

        // Act
        var client = config.CreateHttpClient();

        // Assert
        client.Should().NotBeNull();
        client.Timeout.Should().Be(TimeSpan.FromSeconds(45));
    }

    [Fact]
    public void GetHttpClient_ShouldReturnSameInstance()
    {
        // Arrange
        var config = new ConnectionConfig();

        // Act
        var client1 = config.GetHttpClient();
        var client2 = config.GetHttpClient();

        // Assert
        client1.Should().BeSameAs(client2);
    }

    [Fact]
    public void CreateSseHttpClient_ShouldHaveInfiniteTimeout()
    {
        // Arrange
        var config = new ConnectionConfig();

        // Act
        var client = config.CreateSseHttpClient();

        // Assert
        client.Should().NotBeNull();
        client.Timeout.Should().Be(Timeout.InfiniteTimeSpan);
    }
}
