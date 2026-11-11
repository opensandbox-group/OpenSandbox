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

using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public class DiagnosticsE2ETests : IClassFixture<DiagnosticsE2ETestFixture>
{
    private readonly DiagnosticsE2ETestFixture _fixture;
    public DiagnosticsE2ETests(DiagnosticsE2ETestFixture fixture) => _fixture = fixture;

    [Fact]
    public async Task Context_ShouldIdentifyTheSameBackendThroughBothFacades()
    {
        var context = await _fixture.Manager.GetDiagnosticContextAsync(_fixture.Sandbox.Id);
        var current = await _fixture.Sandbox.GetDiagnosticContextAsync();
        Assert.Equal(_fixture.Sandbox.Id, context.SandboxId);
        Assert.Equal(context.BackendType, current.BackendType);
        Assert.Equal(context.BackendIdentifiers.OrderBy(pair => pair.Key), current.BackendIdentifiers.OrderBy(pair => pair.Key));
        Assert.False(string.IsNullOrEmpty(context.State));
        if (context.BackendType == "docker")
        {
            Assert.False(string.IsNullOrEmpty(context.BackendIdentifiers["containerId"]));
            Assert.False(string.IsNullOrEmpty(context.BackendIdentifiers["containerName"]));
        }
        else
        {
            Assert.Equal("kubernetes", context.BackendType);
            Assert.False(string.IsNullOrEmpty(context.BackendIdentifiers["namespace"]));
            Assert.False(string.IsNullOrEmpty(context.BackendIdentifiers["workloadName"]));
        }
    }

    [Theory]
    [InlineData("container")]
    [InlineData("all")]
    public async Task Logs_ShouldContainEntrypointOutput(string scope)
    {
        foreach (var getLogs in new Func<Task<DiagnosticContent>>[]
        {
            () => _fixture.Manager.GetDiagnosticLogsAsync(_fixture.Sandbox.Id, scope),
            () => _fixture.Sandbox.GetDiagnosticLogsAsync(scope)
        })
        {
            var timer = System.Diagnostics.Stopwatch.StartNew();
            while (true)
            {
                var result = await getLogs();
                AssertContent(result, "logs", scope);
                if (result.Content!.Contains(DiagnosticsE2ETestFixture.Marker)) break;
                Assert.True(timer.Elapsed < TimeSpan.FromSeconds(30), $"entrypoint log missing: {result.Content}");
                await Task.Delay(500);
            }
        }
    }

    [Theory]
    [InlineData("runtime")]
    [InlineData("all")]
    public async Task Events_ShouldReturnDescriptors(string scope)
    {
        AssertContent(await _fixture.Manager.GetDiagnosticEventsAsync(_fixture.Sandbox.Id, scope), "events", scope);
        AssertContent(await _fixture.Sandbox.GetDiagnosticEventsAsync(scope), "events", scope);
    }

    [Fact]
    public async Task UnsupportedScope_ShouldPreserveApiErrors()
    {
        foreach (var call in new Func<Task>[]
        {
            () => _fixture.Manager.GetDiagnosticLogsAsync(_fixture.Sandbox.Id, "unsupported-e2e-scope"),
            () => _fixture.Manager.GetDiagnosticEventsAsync(_fixture.Sandbox.Id, "unsupported-e2e-scope")
        })
        {
            var error = await Assert.ThrowsAsync<SandboxApiException>(call);
            Assert.Equal(400, error.StatusCode);
            Assert.Equal("DIAGNOSTICS_SCOPE_UNSUPPORTED", error.Error.Code);
        }
    }

    [Fact]
    public async Task MissingSandbox_ShouldPreserveNotFound()
    {
        var id = Guid.NewGuid().ToString();
        foreach (var call in new Func<Task>[]
        {
            () => _fixture.Manager.GetDiagnosticContextAsync(id),
            () => _fixture.Manager.GetDiagnosticLogsAsync(id, "container"),
            () => _fixture.Manager.GetDiagnosticEventsAsync(id, "runtime")
        })
        {
            var error = await Assert.ThrowsAsync<SandboxApiException>(call);
            Assert.Equal(404, error.StatusCode);
        }
    }

    private void AssertContent(DiagnosticContent result, string kind, string scope)
    {
        Assert.Equal(_fixture.Sandbox.Id, result.SandboxId);
        Assert.Equal(kind, result.Kind);
        Assert.Equal(scope, result.Scope);
        Assert.Equal("inline", result.Delivery);
        Assert.StartsWith("text/plain", result.ContentType);
        Assert.NotNull(result.Content);
    }
}

public sealed class DiagnosticsE2ETestFixture : IAsyncLifetime
{
    public const string Marker = "opensandbox-diagnostics-e2e";
    private readonly E2ETestFixture _base = new();
    private SandboxManager? _manager;
    private Sandbox? _sandbox;
    public SandboxManager Manager => _manager ?? throw new InvalidOperationException("Manager not initialized");
    public Sandbox Sandbox => _sandbox ?? throw new InvalidOperationException("Sandbox not initialized");

    public async Task InitializeAsync()
    {
        _manager = SandboxManager.Create(new SandboxManagerOptions { ConnectionConfig = _base.ConnectionConfig });
        _sandbox = await OpenSandbox.Sandbox.CreateAsync(new SandboxCreateOptions
        {
            Image = _base.DefaultImage, ConnectionConfig = _base.ConnectionConfig,
            TimeoutSeconds = 300, ReadyTimeoutSeconds = 90,
            Entrypoint = new[] { "sh", "-c", $"echo {Marker}; sleep 300" },
            Env = new Dictionary<string, string> { ["EXECD_API_GRACE_SHUTDOWN"] = "1s" }
        });
    }

    public async Task DisposeAsync()
    {
        try
        {
            if (_sandbox != null)
            {
                try { await _sandbox.KillAsync(); }
                catch (SandboxApiException error) when (error.StatusCode == 404) { }
                finally { await _sandbox.DisposeAsync(); }
            }
        }
        finally { if (_manager != null) await _manager.DisposeAsync(); }
    }
}
