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

using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public class SandboxManagerE2ETests : IClassFixture<SandboxManagerE2ETestFixture>
{
    private readonly SandboxManagerE2ETestFixture _fixture;

    public SandboxManagerE2ETests(SandboxManagerE2ETestFixture fixture)
    {
        _fixture = fixture;
    }

    [Fact(Timeout = 10 * 60 * 1000)]
    public async Task ListSandboxInfos_StatesFilter_IsOrLogic()
    {
        var manager = _fixture.Manager;
        var tag = _fixture.Tag;
        var s1 = _fixture.S1;
        var s2 = _fixture.S2;
        var s3 = _fixture.S3;

        var result = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            States = new[] { SandboxStates.Running, SandboxStates.Paused },
            Metadata = new Dictionary<string, string> { ["tag"] = tag },
            PageSize = 50
        });

        var ids = result.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s1.Id, ids);
        Assert.Contains(s2.Id, ids);
        Assert.Contains(s3.Id, ids);

        var pausedOnly = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            States = new[] { SandboxStates.Paused },
            Metadata = new Dictionary<string, string> { ["tag"] = tag },
            PageSize = 50
        });

        var pausedIds = pausedOnly.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s3.Id, pausedIds);
        Assert.DoesNotContain(s1.Id, pausedIds);
        Assert.DoesNotContain(s2.Id, pausedIds);

        var runningOnly = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            States = new[] { SandboxStates.Running },
            Metadata = new Dictionary<string, string> { ["tag"] = tag },
            PageSize = 50
        });

        var runningIds = runningOnly.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s1.Id, runningIds);
        Assert.Contains(s2.Id, runningIds);
        Assert.DoesNotContain(s3.Id, runningIds);
    }

    [Fact(Timeout = 10 * 60 * 1000)]
    public async Task ListSandboxInfos_MetadataFilter_IsAndLogic()
    {
        var manager = _fixture.Manager;
        var tag = _fixture.Tag;
        var s1 = _fixture.S1;
        var s2 = _fixture.S2;
        var s3 = _fixture.S3;

        var tagAndTeam = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            Metadata = new Dictionary<string, string> { ["tag"] = tag, ["team"] = "t1" },
            PageSize = 50
        });

        var tagAndTeamIds = tagAndTeam.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s1.Id, tagAndTeamIds);
        Assert.Contains(s2.Id, tagAndTeamIds);
        Assert.DoesNotContain(s3.Id, tagAndTeamIds);

        var tagTeamEnv = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            Metadata = new Dictionary<string, string>
            {
                ["tag"] = tag,
                ["team"] = "t1",
                ["env"] = "prod"
            },
            PageSize = 50
        });

        var tagTeamEnvIds = tagTeamEnv.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s1.Id, tagTeamEnvIds);
        Assert.DoesNotContain(s2.Id, tagTeamEnvIds);
        Assert.DoesNotContain(s3.Id, tagTeamEnvIds);

        var tagEnv = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            Metadata = new Dictionary<string, string>
            {
                ["tag"] = tag,
                ["env"] = "prod"
            },
            PageSize = 50
        });

        var tagEnvIds = tagEnv.Items.Select(info => info.Id).ToHashSet();
        Assert.Contains(s1.Id, tagEnvIds);
        Assert.Contains(s3.Id, tagEnvIds);
        Assert.DoesNotContain(s2.Id, tagEnvIds);

        var noneMatch = await manager.ListSandboxInfosAsync(new SandboxFilter
        {
            Metadata = new Dictionary<string, string>
            {
                ["tag"] = tag,
                ["team"] = "t2"
            },
            PageSize = 50
        });
        var createdIds = new HashSet<string> { s1.Id, s2.Id, s3.Id };
        Assert.DoesNotContain(noneMatch.Items, info => createdIds.Contains(info.Id));

        var patched = await manager.PatchSandboxMetadataAsync(s2.Id, new Dictionary<string, string?>
        {
            ["env"] = "stage",
            ["team"] = null
        });
        Assert.Equal("stage", patched.Metadata!["env"]);
        Assert.False(patched.Metadata!.ContainsKey("team"));

        var refreshed = await manager.GetSandboxInfoAsync(s2.Id);
        Assert.Equal("stage", refreshed.Metadata!["env"]);
        Assert.False(refreshed.Metadata!.ContainsKey("team"));
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Manager_InvalidSandboxOperations_ShouldFail()
    {
        var manager = _fixture.Manager;
        var fakeId = $"sandbox-not-exist-{Guid.NewGuid():N}";

        await Assert.ThrowsAnyAsync<Exception>(() => manager.GetSandboxInfoAsync(fakeId));
        await Assert.ThrowsAnyAsync<Exception>(() => manager.PauseSandboxAsync(fakeId));
        await Assert.ThrowsAnyAsync<Exception>(() => manager.ResumeSandboxAsync(fakeId));
        await Assert.ThrowsAnyAsync<Exception>(() => manager.KillSandboxAsync(fakeId));
        await Assert.ThrowsAnyAsync<Exception>(() => manager.RenewSandboxAsync(fakeId, 300));
    }
}

public sealed class SandboxManagerE2ETestFixture : IAsyncLifetime
{
    private readonly E2ETestFixture _baseFixture = new();
    private SandboxManager? _manager;
    private Sandbox? _s1;
    private Sandbox? _s2;
    private Sandbox? _s3;
    private string? _tag;

    public SandboxManager Manager => _manager ?? throw new InvalidOperationException("Manager is not initialized.");
    public Sandbox S1 => _s1 ?? throw new InvalidOperationException("S1 is not initialized.");
    public Sandbox S2 => _s2 ?? throw new InvalidOperationException("S2 is not initialized.");
    public Sandbox S3 => _s3 ?? throw new InvalidOperationException("S3 is not initialized.");
    public string Tag => _tag ?? throw new InvalidOperationException("Tag is not initialized.");

    public async Task InitializeAsync()
    {
        _manager = SandboxManager.Create(new SandboxManagerOptions
        {
            ConnectionConfig = _baseFixture.ConnectionConfig
        });

        _tag = $"csharp-manager-{Guid.NewGuid():N}"[..20];

        _s1 = await CreateSandboxAsync(new Dictionary<string, string>
        {
            ["tag"] = _tag,
            ["team"] = "t1",
            ["env"] = "prod"
        });

        _s2 = await CreateSandboxAsync(new Dictionary<string, string>
        {
            ["tag"] = _tag,
            ["team"] = "t1",
            ["env"] = "dev"
        });

        _s3 = await CreateSandboxAsync(new Dictionary<string, string>
        {
            ["tag"] = _tag,
            ["env"] = "prod"
        });

        await _manager.PauseSandboxAsync(_s3.Id);
        await WaitForStateAsync(_s3.Id, SandboxStates.Paused, TimeSpan.FromMinutes(3));
    }

    public async Task DisposeAsync()
    {
        foreach (var sandbox in new[] { _s1, _s2, _s3 })
        {
            if (sandbox == null)
            {
                continue;
            }

            try
            {
                await sandbox.KillAsync();
            }
            catch
            {
            }

            await sandbox.DisposeAsync();
        }

        if (_manager != null)
        {
            await _manager.DisposeAsync();
        }
    }

    private async Task<Sandbox> CreateSandboxAsync(IReadOnlyDictionary<string, string> metadata)
    {
        return await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _baseFixture.ConnectionConfig,
            Image = _baseFixture.DefaultImage,
            TimeoutSeconds = _baseFixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _baseFixture.DefaultReadyTimeoutSeconds,
            Metadata = metadata,
            Env = new Dictionary<string, string> { ["E2E_TEST"] = "true", ["EXECD_API_GRACE_SHUTDOWN"] = "3s", ["EXECD_JUPYTER_IDLE_POLL_INTERVAL"] = "200ms" },
            HealthCheckPollingInterval = 500
        });
    }

    private async Task WaitForStateAsync(string sandboxId, string expectedState, TimeSpan timeout)
    {
        var deadline = DateTime.UtcNow + timeout;
        while (true)
        {
            var info = await Manager.GetSandboxInfoAsync(sandboxId);
            if (info.Status.State == expectedState)
            {
                return;
            }

            if (DateTime.UtcNow > deadline)
            {
                throw new TimeoutException($"Timed out waiting for state={expectedState}, last_state={info.Status.State}");
            }

            await Task.Delay(1000);
        }
    }
}
