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

using System.Net;
using System.Text;
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using OpenSandbox.Core;
using OpenSandbox.Config;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Moq;
using Xunit;

namespace OpenSandbox.Tests;

public class DiagnosticsTests
{
    [Fact]
    public async Task PublicFacades_ShouldUseInjectedDiagnosticsWithoutExecdCalls()
    {
        var diagnostics = new Mock<IDiagnostics>(MockBehavior.Strict);
        var expected = new DiagnosticContext
        {
            SandboxId = "sbx-1", BackendType = "docker",
            BackendIdentifiers = new Dictionary<string, string>()
        };
        diagnostics.Setup(service => service.GetContextAsync("sbx-1", default)).ReturnsAsync(expected);
        var sandboxes = new Mock<ISandboxes>(MockBehavior.Strict);
        sandboxes.Setup(service => service.GetSandboxEndpointAsync(
            "sbx-1", Constants.DefaultExecdPort, false, It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Endpoint { EndpointAddress = "localhost:44772" });
        var factory = new Mock<IAdapterFactory>(MockBehavior.Strict);
        factory.Setup(value => value.CreateLifecycleStack(It.IsAny<CreateLifecycleStackOptions>()))
            .Returns(new LifecycleStack { Sandboxes = sandboxes.Object, Diagnostics = diagnostics.Object });
        factory.Setup(value => value.CreateExecdStack(It.IsAny<CreateExecdStackOptions>()))
            .Returns(new ExecdStack
            {
                Commands = Mock.Of<IExecdCommands>(), Files = Mock.Of<ISandboxFiles>(),
                Health = Mock.Of<IExecdHealth>(), Metrics = Mock.Of<IExecdMetrics>(),
                Isolation = Mock.Of<IIsolatedSessions>()
            });
        factory.Setup(value => value.CreateEgressStack(It.IsAny<CreateEgressStackOptions>()))
            .Returns(new EgressStack { Egress = Mock.Of<IEgress>() });
        sandboxes.Setup(service => service.GetSandboxEndpointAsync(
            "sbx-1", Constants.DefaultEgressPort, false, It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Endpoint { EndpointAddress = "localhost:28888" });
        var config = new ConnectionConfig(new ConnectionConfigOptions { Domain = "localhost:8080" });
        await using var manager = SandboxManager.Create(new SandboxManagerOptions
        {
            ConnectionConfig = config, AdapterFactory = factory.Object
        });
        (await manager.GetDiagnosticContextAsync("sbx-1")).Should().BeSameAs(expected);
        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sbx-1", ConnectionConfig = config,
            AdapterFactory = factory.Object, SkipHealthCheck = true
        });
        (await sandbox.GetDiagnosticContextAsync()).Should().BeSameAs(expected);
        diagnostics.Verify(service => service.GetContextAsync("sbx-1", default), Times.Exactly(2));
    }

    [Fact]
    public async Task Context_ShouldUseLifecycleAuthenticationAndAllowMissingState()
    {
        using var handler = new DiagnosticHandler();
        using var http = new HttpClient(handler);
        var adapter = new DiagnosticsAdapter(new HttpClientWrapper(
            http, "http://localhost/v1", new Dictionary<string, string> { ["OPEN-SANDBOX-API-KEY"] = "key" }));
        var result = await adapter.GetContextAsync("sbx-1");
        result.BackendType.Should().Be("docker");
        result.State.Should().BeNull();
        result.BackendIdentifiers["containerId"].Should().Be("container-1");
        handler.Path.Should().Be("/v1/sandboxes/sbx-1/diagnostics/context");
        handler.ApiKey.Should().Be("key");
    }

    [Theory]
    [InlineData("logs")]
    [InlineData("events")]
    public async Task Content_ShouldPreserveUrlAndWarningsWithoutDownloading(string kind)
    {
        using var handler = new DiagnosticHandler();
        using var http = new HttpClient(handler);
        var adapter = new DiagnosticsAdapter(new HttpClientWrapper(http, "http://localhost/v1"));
        var result = kind == "logs"
            ? await adapter.GetLogsAsync("sbx-1", "all")
            : await adapter.GetEventsAsync("sbx-1", "all");
        handler.Path.Should().Be($"/v1/sandboxes/sbx-1/diagnostics/{kind}?scope=all");
        handler.Calls.Should().Be(1);
        result.ContentUrl.Should().Be("https://example.invalid/content");
        result.Truncated.Should().BeTrue();
        result.Warnings.Should().Contain("partial");
    }

    [Theory]
    [InlineData(403)]
    [InlineData(404)]
    [InlineData(501)]
    public async Task Context_ShouldPreserveApiErrors(int status)
    {
        using var handler = new DiagnosticHandler { Status = status };
        using var http = new HttpClient(handler);
        var adapter = new DiagnosticsAdapter(new HttpClientWrapper(http, "http://localhost/v1"));
        var action = () => adapter.GetContextAsync("sbx-1");
        await action.Should().ThrowAsync<SandboxApiException>();
    }

    private sealed class DiagnosticHandler : HttpMessageHandler
    {
        public string? Path { get; private set; }
        public string? ApiKey { get; private set; }
        public int Calls { get; private set; }
        public int Status { get; init; } = 200;

        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            Calls++;
            Path = request.RequestUri!.PathAndQuery;
            ApiKey = request.Headers.TryGetValues("OPEN-SANDBOX-API-KEY", out var values) ? values.First() : null;
            var json = Status != 200
                ? """{"code":"TEST_ERROR","message":"Unavailable"}"""
                : Path.EndsWith("/context")
                ? """{"sandboxId":"sbx-1","backendType":"docker","backendIdentifiers":{"containerId":"container-1"}}"""
                : """{"sandboxId":"sbx-1","kind":"logs","scope":"all","delivery":"url","contentType":"text/plain","contentUrl":"https://example.invalid/content","truncated":true,"warnings":["partial"]}""";
            return Task.FromResult(new HttpResponseMessage((HttpStatusCode)Status)
            {
                Content = new StringContent(json, Encoding.UTF8, "application/json")
            });
        }
    }
}
