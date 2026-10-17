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

using Moq;
using OpenSandbox.CodeInterpreter.Factory;
using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Config;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class DataPlaneAuthTests
{
    private const string ApiKeyHeader = "OPEN-SANDBOX-API-KEY";

    [Fact]
    public async Task CreateAsync_DirectMode_OmitsTenantApiKeyFromExecdHeaders()
    {
        var options = await CaptureCodesOptionsAsync(useServerProxy: false);

        Assert.False(options.ExecdHeaders.ContainsKey(ApiKeyHeader));
    }

    [Fact]
    public async Task CreateAsync_ServerProxyMode_RetainsTenantApiKeyInExecdHeaders()
    {
        var options = await CaptureCodesOptionsAsync(useServerProxy: true);

        Assert.Equal("tenant-secret", options.ExecdHeaders[ApiKeyHeader]);
    }

    [Fact]
    public async Task CreateAsync_ServerProxyMode_EndpointApiKeyWinsOverTenantKey()
    {
        var options = await CaptureCodesOptionsAsync(
            useServerProxy: true,
            endpointHeaders: new Dictionary<string, string> { [ApiKeyHeader] = "endpoint-secret" });

        Assert.Equal("endpoint-secret", options.ExecdHeaders[ApiKeyHeader]);
    }

    [Fact]
    public async Task CreateAsync_ServerProxyMode_KeepsExplicitConnectionApiKey()
    {
        var options = await CaptureCodesOptionsAsync(
            useServerProxy: true,
            connectionHeaders: new Dictionary<string, string> { [ApiKeyHeader] = "explicit-secret" });

        Assert.Equal("explicit-secret", options.ExecdHeaders[ApiKeyHeader]);
    }

    private static async Task<CreateCodesStackOptions> CaptureCodesOptionsAsync(
        bool useServerProxy,
        IReadOnlyDictionary<string, string>? endpointHeaders = null,
        IReadOnlyDictionary<string, string>? connectionHeaders = null)
    {
        var captured = new CapturingAdapterFactory();
        var sandbox = await CreateSandboxAsync(useServerProxy, endpointHeaders, connectionHeaders);
        await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
        {
            AdapterFactory = captured,
            SkipHealthCheck = true
        });
        await sandbox.DisposeAsync();

        Assert.NotNull(captured.Options);
        return captured.Options!;
    }

    private static async Task<Sandbox> CreateSandboxAsync(
        bool useServerProxy,
        IReadOnlyDictionary<string, string>? endpointHeaders,
        IReadOnlyDictionary<string, string>? connectionHeaders)
    {
        var sandboxesMock = new Mock<ISandboxes>();
        sandboxesMock
            .Setup(x => x.GetSandboxEndpointAsync(
                It.IsAny<string>(),
                It.IsAny<int>(),
                It.IsAny<bool>(),
                It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Endpoint
            {
                EndpointAddress = "127.0.0.1:44772",
                Headers = endpointHeaders ?? new Dictionary<string, string>()
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
                Health = Mock.Of<IExecdHealth>(),
                Metrics = Mock.Of<IExecdMetrics>(),
                Isolation = Mock.Of<IIsolatedSessions>()
            });

        adapterFactoryMock
            .Setup(x => x.CreateEgressStack(It.IsAny<CreateEgressStackOptions>()))
            .Returns(new EgressStack
            {
                Egress = Mock.Of<IEgress>()
            });

        return await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sbx-code-interpreter-data-plane-auth",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "localhost:8080",
                ApiKey = "tenant-secret",
                UseServerProxy = useServerProxy,
                Headers = connectionHeaders is null
                    ? null
                    : new Dictionary<string, string>(connectionHeaders)
            }),
            AdapterFactory = adapterFactoryMock.Object,
            SkipHealthCheck = true
        });
    }

    private sealed class CapturingAdapterFactory : ICodeInterpreterAdapterFactory
    {
        public CreateCodesStackOptions? Options { get; private set; }

        public ICodes CreateCodes(CreateCodesStackOptions options)
        {
            Options = options;
            return Mock.Of<ICodes>();
        }
    }
}
