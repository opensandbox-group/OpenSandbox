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
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class OptionsTests
{
    [Fact]
    public void SandboxCreateOptions_ShouldStoreProperties()
    {
        // Arrange & Act
        var options = new SandboxCreateOptions
        {
            Image = "ubuntu:latest",
            TimeoutSeconds = 600,
            Entrypoint = new[] { "bash" },
            Env = new Dictionary<string, string> { ["KEY"] = "VALUE" },
            Metadata = new Dictionary<string, string> { ["tag"] = "test" },
            Platform = new PlatformSpec { Os = "linux", Arch = "arm64" },
            Resource = new Dictionary<string, string> { ["cpu"] = "2" },
            SkipHealthCheck = true,
            ReadyTimeoutSeconds = 60,
            HealthCheckPollingInterval = 500
        };

        // Assert
        options.Image.Should().Be("ubuntu:latest");
        options.TimeoutSeconds.Should().Be(600);
        options.Entrypoint.Should().Contain("bash");
        options.Env.Should().ContainKey("KEY");
        options.Metadata.Should().ContainKey("tag");
        options.Platform.Should().NotBeNull();
        options.Platform!.Arch.Should().Be("arm64");
        options.Resource.Should().ContainKey("cpu");
        options.SkipHealthCheck.Should().BeTrue();
        options.ReadyTimeoutSeconds.Should().Be(60);
        options.HealthCheckPollingInterval.Should().Be(500);
    }

    [Fact]
    public void SandboxCreateOptions_WithNetworkPolicy_ShouldStorePolicy()
    {
        // Arrange & Act
        var options = new SandboxCreateOptions
        {
            Image = "python:3.11",
            NetworkPolicy = new NetworkPolicy
            {
                DefaultAction = NetworkRuleAction.Deny,
                Egress = new List<NetworkRule>
                {
                    new() { Action = NetworkRuleAction.Allow, Target = "pypi.org" }
                }
            }
        };

        // Assert
        options.NetworkPolicy.Should().NotBeNull();
        options.NetworkPolicy!.DefaultAction.Should().Be(NetworkRuleAction.Deny);
        options.NetworkPolicy.Egress.Should().HaveCount(1);
    }

    [Fact]
    public void SandboxCreateOptions_WithImageAuth_ShouldStoreAuth()
    {
        // Arrange & Act
        var options = new SandboxCreateOptions
        {
            Image = "private-registry.com/image:tag",
            ImageAuth = new ImageAuth
            {
                Username = "user",
                Password = "pass"
            }
        };

        // Assert
        options.ImageAuth.Should().NotBeNull();
        options.ImageAuth!.Username.Should().Be("user");
        options.ImageAuth.Password.Should().Be("pass");
    }

    [Fact]
    public void SandboxConnectOptions_ShouldStoreProperties()
    {
        // Arrange & Act
        var options = new SandboxConnectOptions
        {
            SandboxId = "sandbox-123",
            SkipHealthCheck = false,
            ReadyTimeoutSeconds = 30,
            HealthCheckPollingInterval = 200
        };

        // Assert
        options.SandboxId.Should().Be("sandbox-123");
        options.SkipHealthCheck.Should().BeFalse();
        options.ReadyTimeoutSeconds.Should().Be(30);
        options.HealthCheckPollingInterval.Should().Be(200);
    }

    [Fact]
    public void SandboxResumeOptions_ShouldStoreProperties()
    {
        // Arrange & Act
        var options = new SandboxResumeOptions
        {
            SkipHealthCheck = true,
            ReadyTimeoutSeconds = 45,
            HealthCheckPollingInterval = 300
        };

        // Assert
        options.SkipHealthCheck.Should().BeTrue();
        options.ReadyTimeoutSeconds.Should().Be(45);
        options.HealthCheckPollingInterval.Should().Be(300);
    }

    [Fact]
    public void WaitUntilReadyOptions_ShouldStoreProperties()
    {
        // Arrange & Act
        var options = new WaitUntilReadyOptions
        {
            ReadyTimeoutSeconds = 60,
            PollingIntervalMillis = 500
        };

        // Assert
        options.ReadyTimeoutSeconds.Should().Be(60);
        options.PollingIntervalMillis.Should().Be(500);
    }

    [Fact]
    public void WaitUntilReadyOptions_WithCustomHealthCheck_ShouldStoreFunction()
    {
        // Arrange
        Func<Sandbox, Task<bool>> healthCheck = async (sbx) =>
        {
            await Task.Delay(1);
            return true;
        };

        // Act
        var options = new WaitUntilReadyOptions
        {
            ReadyTimeoutSeconds = 30,
            PollingIntervalMillis = 200,
            HealthCheck = healthCheck
        };

        // Assert
        options.HealthCheck.Should().NotBeNull();
        options.HealthCheck.Should().BeSameAs(healthCheck);
    }

    [Fact]
    public void SandboxManagerOptions_ShouldStoreProperties()
    {
        // Arrange
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = "api.example.com"
        });

        // Act
        var options = new SandboxManagerOptions
        {
            ConnectionConfig = config
        };

        // Assert
        options.ConnectionConfig.Should().BeSameAs(config);
    }

    [Fact]
    public void SandboxFilter_ShouldStoreProperties()
    {
        // Arrange & Act
        var filter = new SandboxFilter
        {
            States = new[] { "Running", "Paused" },
            Metadata = new Dictionary<string, string> { ["env"] = "test" },
            Page = 1,
            PageSize = 20
        };

        // Assert
        filter.States.Should().HaveCount(2);
        filter.States.Should().Contain("Running");
        filter.States.Should().Contain("Paused");
        filter.Metadata.Should().ContainKey("env");
        filter.Page.Should().Be(1);
        filter.PageSize.Should().Be(20);
    }

    [Fact]
    public void SandboxFilter_WithNullValues_ShouldAllowNulls()
    {
        // Arrange & Act
        var filter = new SandboxFilter();

        // Assert
        filter.States.Should().BeNull();
        filter.Metadata.Should().BeNull();
        filter.Page.Should().BeNull();
        filter.PageSize.Should().BeNull();
    }
}
