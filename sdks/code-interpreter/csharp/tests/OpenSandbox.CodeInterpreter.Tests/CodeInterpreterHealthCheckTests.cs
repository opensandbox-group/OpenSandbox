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

using Moq;
using OpenSandbox.CodeInterpreter.Models;
using OpenSandbox.CodeInterpreter.Services;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Xunit;

namespace OpenSandbox.CodeInterpreter.Tests;

public class CodeInterpreterHealthCheckTests
{
    [Fact]
    public async Task CreateAsync_PingsUntilCodeServiceReady()
    {
        var commands = CommandsMock(executionErrorOnFirstAttempts: 2);
        var sandbox = await CreateConnectedSandboxAsync(commands.Object);
        var codes = new FakeCodes(pingSucceedsAfter: 3);
        var factory = new FakeAdapterFactory(codes);

        var interpreter = await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
        {
            AdapterFactory = factory,
            ReadyTimeoutSeconds = 5,
            HealthCheckPollingInterval = 1
        });

        // Ping fails twice (attempts 1-2, runtime leg skipped), then succeeds from
        // attempt 3; the runtime-process command fails twice (attempts 3-4) and passes
        // on attempt 5. Both legs must pass in the same attempt.
        Assert.Equal(5, codes.PingAttempts);
        commands.Verify(
            x => x.RunAsync(
                CodeInterpreterHealthCheck.RuntimeCheckCommand,
                It.IsAny<RunCommandOptions?>(),
                It.IsAny<ExecutionHandlers?>(),
                It.IsAny<CancellationToken>()),
            Times.Exactly(3));
        Assert.True(await interpreter.IsHealthyAsync());
        await sandbox.DisposeAsync();
    }

    [Fact]
    public async Task CreateAsync_ThrowsWhenCodeServiceNeverReady()
    {
        var sandbox = await CreateConnectedSandboxAsync();
        var codes = new FakeCodes(pingSucceedsAfter: int.MaxValue);
        var factory = new FakeAdapterFactory(codes);

        await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() =>
            CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
            {
                AdapterFactory = factory,
                ReadyTimeoutSeconds = 1,
                HealthCheckPollingInterval = 1
            }));
        await sandbox.DisposeAsync();
    }

    [Fact]
    public async Task CreateAsync_ThrowsWhenRuntimeProcessNeverAppears()
    {
        // Daemon ping passes, but the runtime process check keeps failing.
        var commands = CommandsMock(executionErrorOnFirstAttempts: int.MaxValue);
        var sandbox = await CreateConnectedSandboxAsync(commands.Object);
        var codes = new FakeCodes(pingSucceedsAfter: 1);
        var factory = new FakeAdapterFactory(codes);

        await Assert.ThrowsAsync<SandboxReadyTimeoutException>(() =>
            CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
            {
                AdapterFactory = factory,
                ReadyTimeoutSeconds = 1,
                HealthCheckPollingInterval = 1
            }));
        await sandbox.DisposeAsync();
    }

    [Fact]
    public async Task CreateAsync_RunsRuntimeProcessCheckScript()
    {
        var commands = CommandsMock(executionErrorOnFirstAttempts: 0);
        var sandbox = await CreateConnectedSandboxAsync(commands.Object);
        var codes = new FakeCodes(pingSucceedsAfter: 1);
        var factory = new FakeAdapterFactory(codes);

        var interpreter = await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
        {
            AdapterFactory = factory
        });

        Assert.True(await interpreter.IsHealthyAsync());
        commands.Verify(
            x => x.RunAsync(
                CodeInterpreterHealthCheck.RuntimeCheckCommand,
                It.IsAny<RunCommandOptions?>(),
                It.IsAny<ExecutionHandlers?>(),
                It.IsAny<CancellationToken>()),
            Times.AtLeastOnce);
        await sandbox.DisposeAsync();
    }

    [Fact]
    public async Task CreateAsync_SkipsHealthCheckWhenRequested()
    {
        var commands = CommandsMock(executionErrorOnFirstAttempts: 0);
        var sandbox = await CreateConnectedSandboxAsync(commands.Object);
        var codes = new FakeCodes(pingSucceedsAfter: int.MaxValue);
        var factory = new FakeAdapterFactory(codes);

        var interpreter = await CodeInterpreter.CreateAsync(sandbox, new CodeInterpreterCreateOptions
        {
            AdapterFactory = factory,
            SkipHealthCheck = true
        });

        Assert.Equal(0, codes.PingAttempts);
        Assert.Equal(sandbox.Id, interpreter.Id);
        commands.Verify(
            x => x.RunAsync(
                It.IsAny<string>(),
                It.IsAny<RunCommandOptions?>(),
                It.IsAny<ExecutionHandlers?>(),
                It.IsAny<CancellationToken>()),
            Times.Never);
        await sandbox.DisposeAsync();
    }

    /// <summary>
    /// Returns an <see cref="IExecdCommands"/> mock whose RunAsync succeeds for the
    /// code-interpreter runtime check, failing (error result) for the first
    /// <paramref name="executionErrorOnFirstAttempts"/> calls.
    /// </summary>
    private static Mock<IExecdCommands> CommandsMock(int executionErrorOnFirstAttempts)
    {
        var commands = new Mock<IExecdCommands>();
        var calls = 0;
        commands
            .Setup(x => x.RunAsync(
                It.IsAny<string>(),
                It.IsAny<RunCommandOptions?>(),
                It.IsAny<ExecutionHandlers?>(),
                It.IsAny<CancellationToken>()))
            .ReturnsAsync(() =>
            {
                calls++;
                return new Execution
                {
                    Error = calls <= executionErrorOnFirstAttempts
                        ? new ExecutionError
                        {
                            Name = "CommandExecError",
                            Value = "1",
                            Timestamp = 0,
                            Traceback = Array.Empty<string>()
                        }
                        : null
                };
            });
        return commands;
    }

    private static async Task<Sandbox> CreateConnectedSandboxAsync(IExecdCommands? commands = null)
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
                Commands = commands ?? Mock.Of<IExecdCommands>(),
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
            SandboxId = "sbx-code-interpreter-health",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "localhost:8080"
            }),
            AdapterFactory = adapterFactoryMock.Object,
            SkipHealthCheck = true
        });
    }

    private sealed class FakeAdapterFactory : Factory.ICodeInterpreterAdapterFactory
    {
        private readonly ICodes _codes;

        public FakeAdapterFactory(ICodes codes)
        {
            _codes = codes;
        }

        public ICodes CreateCodes(Factory.CreateCodesStackOptions options)
        {
            return _codes;
        }
    }

    private sealed class FakeCodes : ICodes, IExecdHealth
    {
        private readonly int _pingSucceedsAfter;

        public int PingAttempts { get; private set; }

        public FakeCodes(int pingSucceedsAfter)
        {
            _pingSucceedsAfter = pingSucceedsAfter;
        }

        public Task<bool> PingAsync(CancellationToken cancellationToken = default)
        {
            PingAttempts++;
            return Task.FromResult(PingAttempts >= _pingSucceedsAfter);
        }

        public Task<CodeContext> CreateContextAsync(string language, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<CodeContext> GetContextAsync(string contextId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<IReadOnlyList<CodeContext>> ListContextsAsync(string language, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task DeleteContextAsync(string contextId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task DeleteContextsAsync(string language, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task<Execution> RunAsync(string code, RunCodeOptions? options = null, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public IAsyncEnumerable<ServerStreamEvent> RunStreamAsync(RunCodeRequest request, CancellationToken cancellationToken = default) => throw new NotSupportedException();
        public Task InterruptAsync(string executionId, CancellationToken cancellationToken = default) => throw new NotSupportedException();
    }
}
