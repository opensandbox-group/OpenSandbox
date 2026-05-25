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
using OpenSandbox.Factory;
using OpenSandbox.Models;
using OpenSandbox.Services;
using Microsoft.Extensions.Logging.Abstractions;
using Moq;
using Xunit;

namespace OpenSandbox.Tests;

public class SandboxEgressLifecycleTests
{
    [Fact]
    public async Task CreateAsync_ShouldBuildEgressStackOnce_AndReuseItForOperations()
    {
        var sandboxes = new StubSandboxes();
        var egress = new StubEgress();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress);

        var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            Image = "python:3.12",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "127.0.0.1:8080",
                Protocol = ConnectionProtocol.Http
            }),
            AdapterFactory = adapterFactory,
            SkipHealthCheck = true,
            Diagnostics = new SdkDiagnosticsOptions
            {
                LoggerFactory = NullLoggerFactory.Instance
            }
        });

        await sandbox.GetEgressPolicyAsync();
        await sandbox.PatchEgressRulesAsync([new NetworkRule
        {
            Action = NetworkRuleAction.Allow,
            Target = "www.github.com"
        }]);

        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort, Constants.DefaultEgressPort);
        adapterFactory.EgressStackCallCount.Should().Be(1);
        adapterFactory.LastEgressBaseUrl.Should().Be($"http://127.0.0.1:{Constants.DefaultEgressPort}");
        egress.GetPolicyCallCount.Should().Be(1);
        egress.PatchRulesCallCount.Should().Be(1);
    }

    [Fact]
    public async Task CreateAsync_ShouldAcceptWindowsHostPath()
    {
        var sandboxes = new StubSandboxes();
        var adapterFactory = new StubAdapterFactory(sandboxes, new StubEgress());

        await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            Image = "python:3.12",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "127.0.0.1:8080",
                Protocol = ConnectionProtocol.Http
            }),
            AdapterFactory = adapterFactory,
            SkipHealthCheck = true,
            Volumes =
            [
                new Volume
                {
                    Name = "host-vol",
                    Host = new Host { Path = "D:/sandbox-mnt/ReMe" },
                    MountPath = "/mnt/data"
                }
            ],
            Diagnostics = new SdkDiagnosticsOptions
            {
                LoggerFactory = NullLoggerFactory.Instance
            }
        });

        sandboxes.LastCreateRequest.Should().NotBeNull();
        sandboxes.LastCreateRequest!.Volumes.Should().NotBeNull();
        sandboxes.LastCreateRequest.Volumes!.Should().ContainSingle();
        sandboxes.LastCreateRequest.Volumes![0].Host!.Path.Should().Be("D:/sandbox-mnt/ReMe");
    }

    [Fact]
    public async Task CreateAsync_ShouldSupportSnapshotRestore()
    {
        var sandboxes = new StubSandboxes();
        var adapterFactory = new StubAdapterFactory(sandboxes, new StubEgress());

        await using var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            SnapshotId = "snap-123",
            ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
            {
                Domain = "127.0.0.1:8080",
                Protocol = ConnectionProtocol.Http
            }),
            AdapterFactory = adapterFactory,
            SkipHealthCheck = true,
            Diagnostics = new SdkDiagnosticsOptions
            {
                LoggerFactory = NullLoggerFactory.Instance
            }
        });

        sandboxes.LastCreateRequest.Should().NotBeNull();
        sandboxes.LastCreateRequest!.SnapshotId.Should().Be("snap-123");
        sandboxes.LastCreateRequest.Image.Should().BeNull();
        sandboxes.LastCreateRequest.Entrypoint.Should().BeNull();
    }

    [Fact]
    public async Task CreateAsync_ShouldRejectRelativeHostPath()
    {
        var sandboxes = new StubSandboxes();
        var adapterFactory = new StubAdapterFactory(sandboxes, new StubEgress());

        Func<Task> act = async () =>
        {
            await Sandbox.CreateAsync(new SandboxCreateOptions
            {
                Image = "python:3.12",
                ConnectionConfig = new ConnectionConfig(new ConnectionConfigOptions
                {
                    Domain = "127.0.0.1:8080",
                    Protocol = ConnectionProtocol.Http
                }),
                AdapterFactory = adapterFactory,
                SkipHealthCheck = true,
                Volumes =
                [
                    new Volume
                    {
                        Name = "host-vol",
                        Host = new Host { Path = "relative/path" },
                        MountPath = "/mnt/data"
                    }
                ],
                Diagnostics = new SdkDiagnosticsOptions
                {
                    LoggerFactory = NullLoggerFactory.Instance
                }
            });
        };

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("Host path must be an absolute path starting with '/' or a Windows drive letter*");
        adapterFactory.LifecycleStackCallCount.Should().Be(0);
    }

    private sealed class StubAdapterFactory : IAdapterFactory
    {
        private readonly ISandboxes _sandboxes;
        private readonly IEgress _egress;

        public StubAdapterFactory(ISandboxes sandboxes, IEgress egress)
        {
            _sandboxes = sandboxes;
            _egress = egress;
        }

        public int EgressStackCallCount { get; private set; }
        public int LifecycleStackCallCount { get; private set; }

        public string? LastEgressBaseUrl { get; private set; }

        public LifecycleStack CreateLifecycleStack(CreateLifecycleStackOptions options)
        {
            LifecycleStackCallCount++;
            return new LifecycleStack
            {
                Sandboxes = _sandboxes
            };
        }

        public ExecdStack CreateExecdStack(CreateExecdStackOptions options)
        {
            return new ExecdStack
            {
                Commands = new Mock<IExecdCommands>(MockBehavior.Strict).Object,
                Files = new StubFiles(),
                Health = new StubHealth(),
                Metrics = new StubMetrics()
            };
        }

        public EgressStack CreateEgressStack(CreateEgressStackOptions options)
        {
            EgressStackCallCount++;
            LastEgressBaseUrl = options.EgressBaseUrl;
            return new EgressStack
            {
                Egress = _egress
            };
        }
    }

    private sealed class StubSandboxes : ISandboxes
    {
        public List<int> EndpointCalls { get; } = new();
        public CreateSandboxRequest? LastCreateRequest { get; private set; }

        public Task<CreateSandboxResponse> CreateSandboxAsync(CreateSandboxRequest request, CancellationToken cancellationToken = default)
        {
            LastCreateRequest = request;
            return Task.FromResult(new CreateSandboxResponse
            {
                Id = "sandbox-test-id",
                Status = new SandboxStatus
                {
                    State = "Running"
                },
                CreatedAt = DateTime.UtcNow,
                Entrypoint = ["/bin/sh"]
            });
        }

        public Task<SandboxInfo> GetSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<ListSandboxesResponse> ListSandboxesAsync(ListSandboxesParams? @params = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<SandboxInfo> PatchSandboxMetadataAsync(string sandboxId, IReadOnlyDictionary<string, string?> patch, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task DeleteSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) =>
            Task.CompletedTask;

        public Task PauseSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task ResumeSandboxAsync(string sandboxId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<RenewSandboxExpirationResponse> RenewSandboxExpirationAsync(string sandboxId, RenewSandboxExpirationRequest request, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<SnapshotInfo> CreateSnapshotAsync(string sandboxId, CreateSnapshotRequest? request = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<SnapshotInfo> GetSnapshotAsync(string snapshotId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<ListSnapshotsResponse> ListSnapshotsAsync(ListSnapshotsParams? @params = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task DeleteSnapshotAsync(string snapshotId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<Endpoint> GetSandboxEndpointAsync(string sandboxId, int port, bool useServerProxy = false, CancellationToken cancellationToken = default)
        {
            EndpointCalls.Add(port);
            return Task.FromResult(new Endpoint
            {
                EndpointAddress = $"127.0.0.1:{port}",
                Headers = new Dictionary<string, string>
                {
                    ["X-Port"] = port.ToString()
                }
            });
        }

        public Task<Endpoint> GetSignedSandboxEndpointAsync(string sandboxId, int port, long expires, bool useServerProxy = false, CancellationToken cancellationToken = default)
        {
            EndpointCalls.Add(port);
            return Task.FromResult(new Endpoint
            {
                EndpointAddress = $"127.0.0.1:{port}",
                Headers = new Dictionary<string, string>
                {
                    ["X-Port"] = port.ToString()
                }
            });
        }
    }

    private sealed class StubEgress : IEgress
    {
        public int GetPolicyCallCount { get; private set; }

        public int PatchRulesCallCount { get; private set; }

        public Task<NetworkPolicy> GetPolicyAsync(CancellationToken cancellationToken = default)
        {
            GetPolicyCallCount++;
            return Task.FromResult(new NetworkPolicy
            {
                DefaultAction = NetworkRuleAction.Deny,
                Egress = [new NetworkRule
                {
                    Action = NetworkRuleAction.Allow,
                    Target = "pypi.org"
                }]
            });
        }

        public Task PatchRulesAsync(IReadOnlyList<NetworkRule> rules, CancellationToken cancellationToken = default)
        {
            PatchRulesCallCount++;
            return Task.CompletedTask;
        }
    }

    private sealed class StubFiles : ISandboxFiles
    {
        public Task<IReadOnlyDictionary<string, SandboxFileInfo>> GetFileInfoAsync(IEnumerable<string> paths, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<IReadOnlyList<SandboxFileInfo>> SearchAsync(SearchEntry entry, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task CreateDirectoriesAsync(IEnumerable<CreateDirectoryEntry> entries, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task DeleteDirectoriesAsync(IEnumerable<string> paths, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task WriteFilesAsync(IEnumerable<WriteEntry> entries, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<string> ReadFileAsync(string path, ReadFileOptions? options = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<byte[]> ReadBytesAsync(string path, ReadBytesOptions? options = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public IAsyncEnumerable<byte[]> ReadBytesStreamAsync(string path, ReadBytesOptions? options = null, CancellationToken cancellationToken = default) =>
            AsyncEnumerable.Empty<byte[]>();

        public Task DeleteFilesAsync(IEnumerable<string> paths, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task MoveFilesAsync(IEnumerable<MoveEntry> entries, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task ReplaceContentsAsync(IEnumerable<ContentReplaceEntry> entries, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task SetPermissionsAsync(IEnumerable<SetPermissionEntry> entries, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();
    }

    private sealed class StubHealth : IExecdHealth
    {
        public Task<bool> PingAsync(CancellationToken cancellationToken = default) => Task.FromResult(true);
    }

    private sealed class StubMetrics : IExecdMetrics
    {
        public Task<SandboxMetrics> GetMetricsAsync(CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();
    }
}
