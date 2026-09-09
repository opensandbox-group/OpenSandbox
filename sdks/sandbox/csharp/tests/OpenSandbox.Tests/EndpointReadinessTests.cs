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

using OpenSandbox;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Factory;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Moq;
using Xunit;

namespace OpenSandbox.Tests;

public class EndpointReadinessTests
{
    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task SynchronousCustomOperationFinishesBeforeTimeoutIsReported(bool fails)
    {
        using var budget = new ReadinessBudget(0.05, default);
        var callerThread = Environment.CurrentManagedThreadId;
        var operationThread = 0;
        var finished = false;
        await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() => budget.Run<bool>(_ =>
        {
            operationThread = Environment.CurrentManagedThreadId;
            Thread.Sleep(100);
            finished = true;
            if (fails) throw new InvalidOperationException("late failure");
            return Task.FromResult(true);
        }));
        Assert.Equal(callerThread, operationThread);
        Assert.True(finished);
    }

    [Fact]
    public async Task TimeoutStopsWaitingForUncooperativeTask()
    {
        using var budget = new ReadinessBudget(0.05, default);
        var work = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        try
        {
            await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() => budget.Run(_ => work.Task).WaitAsync(TimeSpan.FromSeconds(2)));
            Assert.False(work.Task.IsCompleted);
        }
        finally
        {
            work.TrySetResult(true);
        }
    }

    private static SandboxApiException Unavailable(string code = "KUBERNETES::POD_IP_NOT_AVAILABLE", int status = 404) =>
        new("starting", statusCode: status, error: new SandboxError(code));

    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task ConnectAndResumeResolveEachEndpointOnceAfterPublication(bool resume)
    {
        var calls = new List<int>();
        var sandboxes = new Mock<ISandboxes>();
        sandboxes.Setup(s => s.ResumeSandboxAsync("sb", It.IsAny<CancellationToken>())).Returns(Task.CompletedTask);
        sandboxes.Setup(s => s.GetSandboxEndpointAsync("sb", It.IsAny<int>(), false, It.IsAny<CancellationToken>()))
            .Returns((string _, int port, bool _, CancellationToken _) =>
            {
                calls.Add(port);
                if (port == Constants.DefaultEgressPort && calls.Count < 4) throw Unavailable();
                return Task.FromResult(new Endpoint { EndpointAddress = "localhost:44772", Headers = new Dictionary<string, string> { ["token"] = "new" } });
            });
        var factory = Factory(sandboxes.Object);
        var options = new SandboxConnectOptions
        {
            SandboxId = "sb", AdapterFactory = factory.Object, ConnectionConfig = new ConnectionConfig(),
            SkipHealthCheck = true, HealthCheckPollingInterval = 1, ReadyTimeoutSeconds = 1
        };
        await using var sandbox = resume ? await Sandbox.ResumeAsync(options) : await Sandbox.ConnectAsync(options);
        Assert.Equal(new[] { Constants.DefaultExecdPort, Constants.DefaultEgressPort, Constants.DefaultEgressPort, Constants.DefaultEgressPort }, calls);
        factory.Verify(f => f.CreateExecdStack(It.Is<CreateExecdStackOptions>(o => o.ExecdHeaders!["token"] == "new")), Times.Once);
    }

    [Theory]
    [InlineData("SANDBOX_NOT_FOUND", 404)]
    [InlineData("KUBERNETES::POD_IP_NOT_AVAILABLE", 401)]
    [InlineData("KUBERNETES::POD_IP_NOT_AVAILABLE", 403)]
    public async Task PermanentErrorsAreUnchanged(string code, int status)
    {
        using var budget = new ReadinessBudget(1, default);
        var error = Unavailable(code, status);
        var calls = 0;
        var actual = await Assert.ThrowsAsync<SandboxApiException>(() => budget.Endpoint<int>(_ => { calls++; throw error; }, 1));
        Assert.Same(error, actual);
        Assert.Equal(1, calls);
    }

    [Fact]
    public async Task TimeoutRetainsLastErrorAndCancellationRetainsToken()
    {
        var error = Unavailable();
        using (var budget = new ReadinessBudget(0.02, default))
        {
            var actual = await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() => budget.Endpoint<int>(_ => throw error, 1));
            Assert.Same(error, actual.InnerException);
        }
        using var caller = new CancellationTokenSource();
        using var cancelled = new ReadinessBudget(1, caller.Token);
        var task = cancelled.Endpoint<int>(_ => { caller.Cancel(); throw error; }, 1000);
        var cancellation = await Assert.ThrowsAnyAsync<OperationCanceledException>(() => task);
        Assert.Equal(caller.Token, cancellation.CancellationToken);
    }

    [Fact]
    public void HealthTimeoutDoesNotReportPreviousEndpointError()
    {
        using var budget = new ReadinessBudget(0, default);
        budget.Record(Unavailable());
        budget.HealthContext("test health context");

        var error = Assert.Throws<SandboxReadyTimeoutException>(() => budget.Remaining());
        Assert.Null(error.InnerException);
        Assert.DoesNotContain("starting", error.Message);
        Assert.Contains("health check timed out", error.Message);
    }

    [Fact]
    public async Task ConnectSharesOneBudgetAcrossEndpointsAndHealth()
    {
        var lifecycle = new Mock<ISandboxes>();
        lifecycle.Setup(s => s.GetSandboxEndpointAsync("sb", It.IsAny<int>(), false, It.IsAny<CancellationToken>()))
            .Returns(async (string _, int port, bool _, CancellationToken token) =>
            {
                if (port == Constants.DefaultExecdPort) await Task.Delay(600, token);
                return new Endpoint { EndpointAddress = "localhost:44772" };
            });
        var health = new Mock<IExecdHealth>();
        health.Setup(h => h.PingAsync(It.IsAny<CancellationToken>())).Returns(async (CancellationToken token) =>
        {
            await Task.Delay(Timeout.Infinite, token);
            return true;
        });
        var started = System.Diagnostics.Stopwatch.StartNew();
        await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() => Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sb", AdapterFactory = Factory(lifecycle.Object, health.Object).Object, ReadyTimeoutSeconds = 1
        }));
        Assert.True(started.ElapsedMilliseconds < 1400);
        health.Verify(h => h.PingAsync(It.IsAny<CancellationToken>()), Times.Once);
    }

    private static Mock<IAdapterFactory> Factory(ISandboxes sandboxes, IExecdHealth? health = null)
    {
        var factory = new Mock<IAdapterFactory>();
        factory.Setup(f => f.CreateLifecycleStack(It.IsAny<CreateLifecycleStackOptions>())).Returns(new LifecycleStack { Sandboxes = sandboxes });
        factory.Setup(f => f.CreateExecdStack(It.IsAny<CreateExecdStackOptions>())).Returns(new ExecdStack
        {
            Commands = Mock.Of<IExecdCommands>(), Files = Mock.Of<ISandboxFiles>(), Health = health ?? Mock.Of<IExecdHealth>(),
            Metrics = Mock.Of<IExecdMetrics>(), Isolation = Mock.Of<IIsolatedSessions>()
        });
        factory.Setup(f => f.CreateEgressStack(It.IsAny<CreateEgressStackOptions>())).Returns(new EgressStack { Egress = Mock.Of<IEgress>() });
        return factory;
    }

    [Theory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task ProbeLocalCancellationIsRetriedByBothEntryPoints(bool standalone)
    {
        var lifecycle = new Mock<ISandboxes>();
        lifecycle.Setup(s => s.GetSandboxEndpointAsync("sb", It.IsAny<int>(), false, It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Endpoint { EndpointAddress = "localhost:44772" });
        var attempts = 0;
        async Task<bool> Probe(Sandbox _)
        {
            if (++attempts == 1)
            {
                using var local = new CancellationTokenSource(10);
                await Task.Delay(1000, local.Token);
            }
            return true;
        }
        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sb", AdapterFactory = Factory(lifecycle.Object).Object,
            ReadyTimeoutSeconds = 1, HealthCheckPollingInterval = 1,
            SkipHealthCheck = standalone, HealthCheck = Probe
        });
        if (standalone)
            await sandbox.WaitUntilReadyAsync(new WaitUntilReadyOptions
            {
                ReadyTimeoutSeconds = 1, PollingIntervalMillis = 1, HealthCheck = Probe
            });
        Assert.Equal(2, attempts);
    }

}
