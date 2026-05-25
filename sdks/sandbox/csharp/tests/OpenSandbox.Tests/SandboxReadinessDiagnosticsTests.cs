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
using Moq;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.Tests;

public class SandboxReadinessDiagnosticsTests
{
    [Fact]
    public async Task WaitUntilReadyAsync_WhenHealthCheckThrows_IncludesLastErrorAndConnectionContext()
    {
        // Arrange
        var healthMock = new Mock<IExecdHealth>();
        healthMock
            .Setup(x => x.PingAsync(It.IsAny<CancellationToken>()))
            .ThrowsAsync(new Exception("connect ECONNREFUSED 127.0.0.1:8080"));

        var sandbox = await CreateSandboxForReadinessTestAsync(healthMock, useServerProxy: false);

        // Act
        Func<Task> action = async () =>
            await sandbox.WaitUntilReadyAsync(new WaitUntilReadyOptions
            {
                ReadyTimeoutSeconds = 1,
                PollingIntervalMillis = 1
            });

        // Assert
        try
        {
            var ex = await action.Should().ThrowAsync<SandboxReadyTimeoutException>();
            ex.Which.Message.Should().Contain("Sandbox health check timed out");
            ex.Which.Message.Should().Contain("Last health check error");
            ex.Which.Message.Should().Contain("domain=localhost:8080");
            ex.Which.Message.Should().Contain("useServerProxy=False");
            ex.Which.Message.Should().Contain("useServerProxy=true");
            ex.Which.Message.Should().Contain("[docker].host_ip");
        }
        finally
        {
            await sandbox.DisposeAsync();
        }
    }

    [Fact]
    public async Task WaitUntilReadyAsync_WhenHealthCheckReturnsFalse_UsesFalseContinuouslyHint()
    {
        // Arrange
        var healthMock = new Mock<IExecdHealth>();
        healthMock
            .Setup(x => x.PingAsync(It.IsAny<CancellationToken>()))
            .ReturnsAsync(false);

        var sandbox = await CreateSandboxForReadinessTestAsync(healthMock, useServerProxy: true);

        // Act
        Func<Task> action = async () =>
            await sandbox.WaitUntilReadyAsync(new WaitUntilReadyOptions
            {
                ReadyTimeoutSeconds = 1,
                PollingIntervalMillis = 1
            });

        // Assert
        try
        {
            var ex = await action.Should().ThrowAsync<SandboxReadyTimeoutException>();
            ex.Which.Message.Should().Contain("Health check returned false continuously.");
            ex.Which.Message.Should().Contain("useServerProxy=True");
            ex.Which.Message.Should().NotContain("[docker].host_ip");
        }
        finally
        {
            await sandbox.DisposeAsync();
        }
    }

    private static async Task<Sandbox> CreateSandboxForReadinessTestAsync(
        Mock<IExecdHealth> healthMock,
        bool useServerProxy)
    {
        var sandboxesMock = new Mock<ISandboxes>();
        sandboxesMock
            .Setup(x => x.GetSandboxEndpointAsync(
                It.IsAny<string>(),
                It.IsAny<int>(),
                useServerProxy,
                It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Endpoint
            {
                EndpointAddress = "127.0.0.1:44772",
                Headers = new Dictionary<string, string>()
            });

        var adapterFactoryMock = new Mock<IAdapterFactory>();
        adapterFactoryMock
            .Setup(x => x.CreateLifecycleStack(It.IsAny<CreateLifecycleStackOptions>()))
            .Returns(new LifecycleStack
            {
                Sandboxes = sandboxesMock.Object
            });

        adapterFactoryMock
            .Setup(x => x.CreateExecdStack(It.IsAny<CreateExecdStackOptions>()))
            .Returns(new ExecdStack
            {
                Commands = Mock.Of<IExecdCommands>(),
                Files = Mock.Of<ISandboxFiles>(),
                Health = healthMock.Object,
                Metrics = Mock.Of<IExecdMetrics>()
            });

        adapterFactoryMock
            .Setup(x => x.CreateEgressStack(It.IsAny<CreateEgressStackOptions>()))
            .Returns(new EgressStack
            {
                Egress = Mock.Of<IEgress>()
            });

        return await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sbx-ready-diagnostics",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "localhost:8080",
                UseServerProxy = useServerProxy
            }),
            AdapterFactory = adapterFactoryMock.Object,
            SkipHealthCheck = true
        });
    }
}
