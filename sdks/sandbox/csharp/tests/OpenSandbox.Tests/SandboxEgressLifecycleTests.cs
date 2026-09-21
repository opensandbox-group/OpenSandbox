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
        var credentialVault = new StubCredentialVault();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress, credentialVault);

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
        await sandbox.DeleteEgressRulesAsync(["www.github.com", "*.blocked.org"]);
        await sandbox.CredentialVault.GetAsync();
        await sandbox.GetCredentialVaultAsync();

        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort, Constants.DefaultEgressPort);
        adapterFactory.EgressStackCallCount.Should().Be(1);
        adapterFactory.LastEgressBaseUrl.Should().Be($"http://127.0.0.1:{Constants.DefaultEgressPort}");
        egress.GetPolicyCallCount.Should().Be(1);
        egress.PatchRulesCallCount.Should().Be(1);
        egress.DeleteRulesCallCount.Should().Be(1);
        credentialVault.GetVaultCallCount.Should().Be(2);
        egress.LastDeleteTargets.Should().Equal("www.github.com", "*.blocked.org");
    }

    [Fact]
    public async Task CreateAsync_ShouldAcceptCustomEgressWithoutCredentialVaultMethods()
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
        Func<Task> act = () => sandbox.CredentialVault.GetAsync();

        egress.GetPolicyCallCount.Should().Be(1);
        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("Credential Vault is not available for this adapter factory*");
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
            CredentialProxy = new CredentialProxyConfig
            {
                Enabled = true
            },
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
        sandboxes.LastCreateRequest!.CredentialProxy.Should().NotBeNull();
        sandboxes.LastCreateRequest!.CredentialProxy!.Enabled.Should().BeTrue();
        sandboxes.LastCreateRequest!.Volumes.Should().NotBeNull();
        sandboxes.LastCreateRequest.Volumes!.Should().ContainSingle();
        sandboxes.LastCreateRequest.Volumes![0].Host!.Path.Should().Be("D:/sandbox-mnt/ReMe");
    }

    [Fact]
    public async Task CreateAsync_ShouldForwardLifecycleHooks()
    {
        var sandboxes = new StubSandboxes();
        var adapterFactory = new StubAdapterFactory(sandboxes, new StubEgress());
        var lifecycle = new SandboxLifecycle
        {
            PreStart = new LifecycleHook
            {
                Command = ["/opt/hooks/restore.sh"],
                TimeoutSeconds = 300
            },
            Periodic =
            [
                new PeriodicLifecycleHook
                {
                    Name = "checkpoint",
                    Schedule = "@hourly",
                    Command = ["/opt/hooks/checkpoint.sh"]
                }
            ]
        };

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
            Diagnostics = new SdkDiagnosticsOptions
            {
                LoggerFactory = NullLoggerFactory.Instance
            },
            Lifecycle = lifecycle
        });

        sandboxes.LastCreateRequest.Should().NotBeNull();
        var forwarded = sandboxes.LastCreateRequest!.Lifecycle;
        forwarded.Should().NotBeNull();
        forwarded!.PreStart.Should().NotBeNull();
        forwarded.PreStart!.Command.Should().Equal("/opt/hooks/restore.sh");
        forwarded.PreStart.TimeoutSeconds.Should().Be(300);
        forwarded.Periodic.Should().ContainSingle();
        forwarded.Periodic![0].Name.Should().Be("checkpoint");
        forwarded.Periodic[0].Schedule.Should().Be("@hourly");
        forwarded.Periodic[0].Command.Should().Equal("/opt/hooks/checkpoint.sh");
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

    [Fact]
    public async Task CreateFromTemplateAsync_ShouldRouteEgressThroughControlPlane()
    {
        var sandboxes = new StubSandboxes();
        var egress = new StubEgress();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress);

        await using var sandbox = await Sandbox.CreateFromTemplateAsync(new SandboxCreateFromTemplateOptions
        {
            TemplateId = "tpl-123",
            TimeoutSeconds = 600,
            Metadata = new Dictionary<string, string> { ["team"] = "platform" },
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
        sandboxes.LastCreateRequest!.TemplateId.Should().Be("tpl-123");
        sandboxes.LastCreateRequest.Timeout.Should().Be(600);
        sandboxes.LastCreateRequest.Image.Should().BeNull();
        sandboxes.LastCreateRequest.SnapshotId.Should().BeNull();
        sandboxes.LastCreateRequest.Entrypoint.Should().BeNull();
        sandboxes.LastCreateRequest.ResourceLimits.Should().BeNull();
        sandboxes.LastCreateRequest.Metadata.Should().NotBeNull();

        // Template-backed sandboxes never resolve the egress sidecar endpoint.
        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort);
        adapterFactory.EgressStackCallCount.Should().Be(0);
        adapterFactory.NetworkPolicyStackCallCount.Should().Be(1);
        adapterFactory.LastNetworkPolicySandboxId.Should().Be("sandbox-test-id");
        egress.GetPolicyCallCount.Should().Be(0);

        sandbox.Origin.Should().Be(SandboxOrigin.Template);
        await sandbox.GetEgressPolicyAsync();
        egress.GetPolicyCallCount.Should().Be(1);

        Func<Task> credentialVault = () => sandbox.CredentialVault.GetAsync();
        Func<Task> credentialVaultProperty = () =>
        {
            _ = sandbox.CredentialVault;
            return Task.CompletedTask;
        };
        await credentialVaultProperty.Should().ThrowAsync<SandboxException>()
            .WithMessage("Credential Vault is not available for template-backed sandboxes*");
        await credentialVault.Should().ThrowAsync<SandboxException>();
    }

    [Fact]
    public async Task CreateFromTemplateAsync_ShouldRejectBlankTemplateId()
    {
        Func<Task> act = async () =>
        {
            await Sandbox.CreateFromTemplateAsync(new SandboxCreateFromTemplateOptions
            {
                TemplateId = "  ",
                TimeoutSeconds = 600
            });
        };

        await act.Should().ThrowAsync<InvalidArgumentException>()
            .WithMessage("TemplateId must be specified.");
    }

    [Fact]
    public async Task CreateAsync_ShouldDetectTemplateOriginFromServerHeader()
    {
        var sandboxes = new StubSandboxes { ExecdEndpointOrigin = SandboxOrigin.Template };
        var egress = new StubEgress();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress);

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

        // The server is authoritative: fsb-prefixed sandboxes report origin=template
        // even when the create used a snapshotId.
        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort);
        adapterFactory.EgressStackCallCount.Should().Be(0);
        adapterFactory.NetworkPolicyStackCallCount.Should().Be(1);
        adapterFactory.LastNetworkPolicySandboxId.Should().Be("sandbox-test-id");
        sandbox.Origin.Should().Be(SandboxOrigin.Template);
    }

    [Fact]
    public async Task ConnectAsync_ShouldRouteEgressByReportedOrigin()
    {
        var sandboxes = new StubSandboxes { ExecdEndpointOrigin = SandboxOrigin.Template };
        var egress = new StubEgress();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress);

        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sandbox-test-id",
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

        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort);
        adapterFactory.EgressStackCallCount.Should().Be(0);
        adapterFactory.NetworkPolicyStackCallCount.Should().Be(1);
        adapterFactory.LastNetworkPolicySandboxId.Should().Be("sandbox-test-id");
        sandbox.Origin.Should().Be(SandboxOrigin.Template);
    }

    [Fact]
    public async Task ConnectAsync_ShouldKeepSidecarEgressWhenOriginMissing()
    {
        var sandboxes = new StubSandboxes();
        var egress = new StubEgress();
        var credentialVault = new StubCredentialVault();
        var adapterFactory = new StubAdapterFactory(sandboxes, egress, credentialVault);

        await using var sandbox = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            SandboxId = "sandbox-test-id",
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

        sandboxes.EndpointCalls.Should().Equal(Constants.DefaultExecdPort, Constants.DefaultEgressPort);
        adapterFactory.EgressStackCallCount.Should().Be(1);
        adapterFactory.NetworkPolicyStackCallCount.Should().Be(0);
        sandbox.Origin.Should().Be(SandboxOrigin.Unknown);
    }

    private sealed class StubAdapterFactory : IAdapterFactory
    {
        private readonly ISandboxes _sandboxes;
        private readonly IEgress _egress;
        private readonly ICredentialVault? _credentialVault;

        public StubAdapterFactory(
            ISandboxes sandboxes,
            IEgress egress,
            ICredentialVault? credentialVault = null)
        {
            _sandboxes = sandboxes;
            _egress = egress;
            _credentialVault = credentialVault;
        }

        public int EgressStackCallCount { get; private set; }
        public int LifecycleStackCallCount { get; private set; }
        public int NetworkPolicyStackCallCount { get; private set; }

        public string? LastEgressBaseUrl { get; private set; }

        public string? LastNetworkPolicySandboxId { get; private set; }

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
                Metrics = new StubMetrics(),
                Isolation = new Mock<IIsolatedSessions>(MockBehavior.Strict).Object
            };
        }

        public EgressStack CreateEgressStack(CreateEgressStackOptions options)
        {
            EgressStackCallCount++;
            LastEgressBaseUrl = options.EgressBaseUrl;
            return new EgressStack
            {
                Egress = _egress,
                CredentialVault = _credentialVault
            };
        }

        public NetworkPolicyStack CreateNetworkPolicyStack(CreateNetworkPolicyStackOptions options)
        {
            NetworkPolicyStackCallCount++;
            LastNetworkPolicySandboxId = options.SandboxId;
            return new NetworkPolicyStack
            {
                Egress = _egress
            };
        }
    }

    private sealed class StubSandboxes : ISandboxes
    {
        public List<int> EndpointCalls { get; } = new();
        public CreateSandboxRequest? LastCreateRequest { get; private set; }

        /// <summary>
        /// Origin reported by the execd endpoint fetch; null means the server
        /// sends no OPEN-SANDBOX-ORIGIN header.
        /// </summary>
        public string? ExecdEndpointOrigin { get; set; }

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
                },
                Origin = port == Constants.DefaultExecdPort ? ExecdEndpointOrigin : null
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

        public Task<TemplateInfo> CreateTemplateAsync(CreateTemplateRequest request, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<TemplateInfo> GetTemplateAsync(string templateId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task<ListTemplatesResponse> ListTemplatesAsync(ListTemplatesParams? @params = null, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

        public Task DeleteTemplateAsync(string templateId, CancellationToken cancellationToken = default) =>
            throw new NotImplementedException();

    }

    private sealed class StubEgress : IEgress
    {
        public int GetPolicyCallCount { get; private set; }

        public int PatchRulesCallCount { get; private set; }

        public int DeleteRulesCallCount { get; private set; }

        public IReadOnlyList<string> LastDeleteTargets { get; private set; } = [];

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

        public Task DeleteRulesAsync(IReadOnlyList<string> targets, CancellationToken cancellationToken = default)
        {
            DeleteRulesCallCount++;
            LastDeleteTargets = targets.ToList();
            return Task.CompletedTask;
        }
    }

    private sealed class StubCredentialVault : ICredentialVault
    {
        public int GetVaultCallCount { get; private set; }

        public Task<CredentialVaultState> CreateAsync(
            IReadOnlyList<Credential> credentials,
            IReadOnlyList<CredentialBinding> bindings,
            CancellationToken cancellationToken = default)
        {
            return Task.FromResult(CreateVaultState());
        }

        public Task<CredentialVaultState> GetAsync(CancellationToken cancellationToken = default)
        {
            GetVaultCallCount++;
            return Task.FromResult(CreateVaultState());
        }

        public Task<CredentialVaultState> PatchAsync(
            CredentialVaultPatchRequest request,
            CancellationToken cancellationToken = default)
        {
            return Task.FromResult(CreateVaultState());
        }

        public Task DeleteAsync(CancellationToken cancellationToken = default) => Task.CompletedTask;

        public Task<IReadOnlyList<CredentialMetadata>> ListCredentialsAsync(CancellationToken cancellationToken = default)
        {
            return Task.FromResult<IReadOnlyList<CredentialMetadata>>(CreateVaultState().Credentials);
        }

        public Task<CredentialMetadata> GetCredentialAsync(
            string name,
            CancellationToken cancellationToken = default)
        {
            return Task.FromResult(CreateVaultState().Credentials[0]);
        }

        public Task<IReadOnlyList<CredentialBindingMetadata>> ListBindingsAsync(CancellationToken cancellationToken = default)
        {
            return Task.FromResult<IReadOnlyList<CredentialBindingMetadata>>(CreateVaultState().Bindings);
        }

        public Task<CredentialBindingMetadata> GetBindingAsync(
            string name,
            CancellationToken cancellationToken = default)
        {
            return Task.FromResult(CreateVaultState().Bindings[0]);
        }

        private static CredentialVaultState CreateVaultState()
        {
            return new CredentialVaultState
            {
                Revision = 1,
                Credentials =
                [
                    new CredentialMetadata
                    {
                        Name = "api-token",
                        SourceType = "inline",
                        Revision = 1
                    }
                ],
                Bindings =
                [
                    new CredentialBindingMetadata
                    {
                        Name = "api-binding",
                        Revision = 1,
                        Auth = new CredentialAuthMetadata
                        {
                            Type = "bearer"
                        }
                    }
                ]
            };
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

        public Task<IReadOnlyList<SandboxFileInfo>> ListDirectoryAsync(string path, int? depth = null, CancellationToken cancellationToken = default) =>
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

        public Task<IReadOnlyList<ContentReplaceResult>> ReplaceContentsDetailedAsync(IEnumerable<ContentReplaceEntry> entries, CancellationToken cancellationToken = default) =>
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
