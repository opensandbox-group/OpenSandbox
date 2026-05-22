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

using System.Collections.Concurrent;
using System.Text;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public class SandboxE2ETests : IClassFixture<SandboxE2ETestFixture>
{
    private readonly SandboxE2ETestFixture _fixture;

    public SandboxE2ETests(SandboxE2ETestFixture fixture)
    {
        _fixture = fixture;
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Lifecycle_Health_Endpoint_Metrics_Renew_Connect()
    {
        var sandbox = _fixture.Sandbox;
        Assert.False(string.IsNullOrWhiteSpace(sandbox.Id));
        Assert.True(await sandbox.IsHealthyAsync());

        var info = await sandbox.GetInfoAsync();
        Assert.Equal(sandbox.Id, info.Id);
        Assert.Equal(SandboxStates.Running, info.Status.State);
        Assert.Equal(Constants.DefaultEntrypoint, info.Entrypoint);
        Assert.NotNull(info.Metadata);
        Assert.Equal("csharp-e2e-test", info.Metadata!["tag"]);
        Assert.True(info.ExpiresAt > info.CreatedAt);

        var endpoint = await sandbox.GetEndpointAsync(Constants.DefaultExecdPort);
        AssertEndpointHasPort(endpoint.EndpointAddress, Constants.DefaultExecdPort);

        var metrics = await sandbox.GetMetricsAsync();
        Assert.True(metrics.CpuCount > 0);
        Assert.True(metrics.CpuUsedPercentage is >= 0.0 and <= 100.0);
        Assert.True(metrics.MemoryTotalMiB > 0);
        Assert.True(metrics.MemoryUsedMiB <= metrics.MemoryTotalMiB);
        AssertRecentTimestampMs(metrics.Timestamp, 120_000);

        var renewResponse = await sandbox.RenewAsync(30 * 60);
        Assert.NotNull(renewResponse);
        Assert.NotNull(renewResponse.ExpiresAt);
        var renewedInfo = await sandbox.GetInfoAsync();
        Assert.True(renewedInfo.ExpiresAt > info.ExpiresAt);
        Assert.True(renewResponse.ExpiresAt > info.ExpiresAt);

        var sandbox2 = await Sandbox.ConnectAsync(new SandboxConnectOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            SandboxId = sandbox.Id
        });

        try
        {
            Assert.Equal(sandbox.Id, sandbox2.Id);
            Assert.True(await sandbox2.IsHealthyAsync());
            var result = await sandbox2.Commands.RunAsync("echo connect-ok");
            Assert.Null(result.Error);
            Assert.Single(result.Logs.Stdout);
            Assert.Equal("connect-ok", result.Logs.Stdout[0].Text);
        }
        finally
        {
            await sandbox2.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_XRequestId_Passthrough_OnServerError()
    {
        var requestId = $"e2e-csharp-server-{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        var missingSandboxId = $"missing-{requestId}";
        var baseConfig = _fixture.ConnectionConfig;
        var config = new ConnectionConfig(new ConnectionConfigOptions
        {
            Domain = baseConfig.Domain,
            Protocol = baseConfig.Protocol,
            ApiKey = baseConfig.ApiKey,
            RequestTimeoutSeconds = baseConfig.RequestTimeoutSeconds,
            Headers = new Dictionary<string, string> { ["X-Request-ID"] = requestId }
        });

        var ex = await Assert.ThrowsAsync<SandboxApiException>(async () =>
        {
            var connected = await Sandbox.ConnectAsync(new SandboxConnectOptions
            {
                ConnectionConfig = config,
                SandboxId = missingSandboxId
            });
            try
            {
                await connected.GetInfoAsync();
            }
            finally
            {
                await connected.DisposeAsync();
            }
        });

        Assert.Equal(requestId, ex.RequestId);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_ManualCleanup_Returns_Null_ExpiresAt()
    {
        var sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            ManualCleanup = true,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Metadata = new Dictionary<string, string> { ["tag"] = "manual-csharp-e2e-test" }
        });

        try
        {
            var info = await sandbox.GetInfoAsync();
            Assert.Null(info.ExpiresAt);
            Assert.NotNull(info.Metadata);
            Assert.Equal("manual-csharp-e2e-test", info.Metadata!["tag"]);
        }
        finally
        {
            await sandbox.KillAsync();
            await sandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_NetworkPolicy_Get_And_Patch_Egress()
    {
        var policySandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            NetworkPolicy = new NetworkPolicy
            {
                DefaultAction = NetworkRuleAction.Deny,
                Egress = new List<NetworkRule> { new() { Action = NetworkRuleAction.Allow, Target = "pypi.org" } }
            }
        });

        try
        {
            await WaitUntilEgressBlocksAsync(policySandbox, "https://www.github.com", TimeSpan.FromSeconds(30));

            var initialPolicy = await policySandbox.GetEgressPolicyAsync();
            Assert.NotNull(initialPolicy);
            Assert.Equal(NetworkRuleAction.Deny, initialPolicy.DefaultAction);
            Assert.NotNull(initialPolicy.Egress);
            Assert.Contains(
                initialPolicy.Egress!,
                rule => rule.Target == "pypi.org" && rule.Action == NetworkRuleAction.Allow);

            var blocked = await policySandbox.Commands.RunAsync("curl -I https://www.github.com");
            Assert.NotNull(blocked.Error);

            var allowed = await policySandbox.Commands.RunAsync("curl -I https://pypi.org");
            Assert.Null(allowed.Error);

            await policySandbox.PatchEgressRulesAsync(new List<NetworkRule>
            {
                new() { Action = NetworkRuleAction.Allow, Target = "www.github.com" },
                new() { Action = NetworkRuleAction.Deny, Target = "pypi.org" }
            });
            await WaitUntilEgressBlocksAsync(policySandbox, "https://pypi.org", TimeSpan.FromSeconds(30));

            var patchedPolicy = await policySandbox.GetEgressPolicyAsync();
            Assert.NotNull(patchedPolicy.Egress);
            Assert.Contains(
                patchedPolicy.Egress!,
                rule => rule.Target == "www.github.com" && rule.Action == NetworkRuleAction.Allow);
            Assert.Contains(
                patchedPolicy.Egress!,
                rule => rule.Target == "pypi.org" && rule.Action == NetworkRuleAction.Deny);

            var githubAllowed = await policySandbox.Commands.RunAsync("curl -I https://www.github.com");
            Assert.Null(githubAllowed.Error);

            var pypiDenied = await policySandbox.Commands.RunAsync("curl -I https://pypi.org");
            Assert.NotNull(pypiDenied.Error);
        }
        finally
        {
            try
            {
                await policySandbox.KillAsync();
            }
            catch
            {
            }

            await policySandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_NetworkPolicy_Get_And_Patch_Egress_Via_ServerProxy()
    {
        var policySandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ServerProxyConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            NetworkPolicy = new NetworkPolicy
            {
                DefaultAction = NetworkRuleAction.Deny,
                Egress = new List<NetworkRule> { new() { Action = NetworkRuleAction.Allow, Target = "pypi.org" } }
            }
        });

        try
        {
            await WaitUntilEgressBlocksAsync(policySandbox, "https://www.github.com", TimeSpan.FromSeconds(30));

            var egressEndpoint = await policySandbox.GetEndpointAsync(Constants.DefaultEgressPort);
            Assert.Contains(
                $"/sandboxes/{policySandbox.Id}/proxy/{Constants.DefaultEgressPort}",
                egressEndpoint.EndpointAddress);

            var initialPolicy = await policySandbox.GetEgressPolicyAsync();
            Assert.NotNull(initialPolicy);
            Assert.Equal(NetworkRuleAction.Deny, initialPolicy.DefaultAction);
            Assert.NotNull(initialPolicy.Egress);
            Assert.Contains(
                initialPolicy.Egress!,
                rule => rule.Target == "pypi.org" && rule.Action == NetworkRuleAction.Allow);

            var blocked = await policySandbox.Commands.RunAsync("curl -I https://www.github.com");
            Assert.NotNull(blocked.Error);

            var allowed = await policySandbox.Commands.RunAsync("curl -I https://pypi.org");
            Assert.Null(allowed.Error);

            await policySandbox.PatchEgressRulesAsync(new List<NetworkRule>
            {
                new() { Action = NetworkRuleAction.Allow, Target = "www.github.com" },
                new() { Action = NetworkRuleAction.Deny, Target = "pypi.org" }
            });
            await WaitUntilEgressBlocksAsync(policySandbox, "https://pypi.org", TimeSpan.FromSeconds(30));

            var patchedPolicy = await policySandbox.GetEgressPolicyAsync();
            Assert.NotNull(patchedPolicy.Egress);
            Assert.Contains(
                patchedPolicy.Egress!,
                rule => rule.Target == "www.github.com" && rule.Action == NetworkRuleAction.Allow);
            Assert.Contains(
                patchedPolicy.Egress!,
                rule => rule.Target == "pypi.org" && rule.Action == NetworkRuleAction.Deny);
        }
        finally
        {
            try
            {
                await policySandbox.KillAsync();
            }
            catch
            {
            }

            await policySandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_HostVolumeMount()
    {
        var hostDir = "/tmp/opensandbox-e2e/host-volume-test";
        var containerMountPath = "/mnt/host-data";
        var volumeSandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Volumes = new[]
            {
                new Volume
                {
                    Name = "test-host-vol",
                    Host = new Host { Path = hostDir },
                    MountPath = containerMountPath,
                    ReadOnly = false
                }
            }
        });

        try
        {
            // Retry: bind mount propagation can sometimes lag on first access
            var marker = await RunWithRetryAsync(volumeSandbox, $"cat {containerMountPath}/marker.txt");
            Assert.Null(marker.Error);
            Assert.Single(marker.Logs.Stdout);
            Assert.Equal("opensandbox-e2e-marker", marker.Logs.Stdout[0].Text);

            var write = await volumeSandbox.Commands.RunAsync(
                $"echo 'written-from-sandbox' > {containerMountPath}/sandbox-output.txt");
            Assert.Null(write.Error);

            // Retry: bind mount propagation can sometimes lag on first access
            var readBack = await RunWithRetryAsync(volumeSandbox, $"cat {containerMountPath}/sandbox-output.txt");
            Assert.Null(readBack.Error);
            Assert.Single(readBack.Logs.Stdout);
            Assert.Equal("written-from-sandbox", readBack.Logs.Stdout[0].Text);
        }
        finally
        {
            try
            {
                await volumeSandbox.KillAsync();
            }
            catch
            {
            }

            await volumeSandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_HostVolumeMount_ReadOnly()
    {
        var hostDir = "/tmp/opensandbox-e2e/host-volume-test";
        var containerMountPath = "/mnt/host-data-ro";
        var roSandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Volumes = new[]
            {
                new Volume
                {
                    Name = "test-host-vol-ro",
                    Host = new Host { Path = hostDir },
                    MountPath = containerMountPath,
                    ReadOnly = true
                }
            }
        });

        try
        {
            // Retry: bind mount propagation can sometimes lag on first access
            var marker = await RunWithRetryAsync(roSandbox, $"cat {containerMountPath}/marker.txt");
            Assert.Null(marker.Error);
            Assert.Single(marker.Logs.Stdout);
            Assert.Equal("opensandbox-e2e-marker", marker.Logs.Stdout[0].Text);

            var write = await roSandbox.Commands.RunAsync($"touch {containerMountPath}/should-fail.txt");
            var stat = await roSandbox.Commands.RunAsync(
                $"test ! -e {containerMountPath}/should-fail.txt && echo OK");
            var writeWasRejected = write.Error is not null || write.Logs.Stderr.Count > 0;
            var fileWasNotCreated =
                stat.Error is null &&
                stat.Logs.Stdout.Count == 1 &&
                stat.Logs.Stdout[0].Text == "OK";
            Assert.True(
                writeWasRejected || fileWasNotCreated,
                "Write on read-only host volume should fail or leave no created file.");
        }
        finally
        {
            try
            {
                await roSandbox.KillAsync();
            }
            catch
            {
            }

            await roSandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_PvcVolumeMount()
    {
        var pvcVolumeName = "opensandbox-e2e-pvc-test";
        var containerMountPath = "/mnt/pvc-data";
        var pvcSandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Volumes = new[]
            {
                new Volume
                {
                    Name = "test-pvc-vol",
                    Pvc = new PVC { ClaimName = pvcVolumeName },
                    MountPath = containerMountPath,
                    ReadOnly = false
                }
            }
        });

        try
        {
            // Retry: bind mount propagation can sometimes lag on first access
            var marker = await RunWithRetryAsync(pvcSandbox, $"cat {containerMountPath}/marker.txt");
            Assert.Null(marker.Error);
            Assert.Single(marker.Logs.Stdout);
            Assert.Equal("pvc-marker-data", marker.Logs.Stdout[0].Text);

            var write = await pvcSandbox.Commands.RunAsync(
                $"echo 'written-to-pvc' > {containerMountPath}/pvc-output.txt");
            Assert.Null(write.Error);

            // Retry: bind mount propagation can sometimes lag on first access
            var readBack = await RunWithRetryAsync(pvcSandbox, $"cat {containerMountPath}/pvc-output.txt");
            Assert.Null(readBack.Error);
            Assert.Single(readBack.Logs.Stdout);
            Assert.Equal("written-to-pvc", readBack.Logs.Stdout[0].Text);
        }
        finally
        {
            try
            {
                await pvcSandbox.KillAsync();
            }
            catch
            {
            }

            await pvcSandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_PvcVolumeMount_ReadOnly()
    {
        var pvcVolumeName = "opensandbox-e2e-pvc-test";
        var containerMountPath = "/mnt/pvc-data-ro";
        var roSandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Volumes = new[]
            {
                new Volume
                {
                    Name = "test-pvc-vol-ro",
                    Pvc = new PVC { ClaimName = pvcVolumeName },
                    MountPath = containerMountPath,
                    ReadOnly = true
                }
            }
        });

        try
        {
            // Retry: bind mount propagation can sometimes lag on first access
            var marker = await RunWithRetryAsync(roSandbox, $"cat {containerMountPath}/marker.txt");
            Assert.Null(marker.Error);
            Assert.Single(marker.Logs.Stdout);
            Assert.Equal("pvc-marker-data", marker.Logs.Stdout[0].Text);

            var write = await roSandbox.Commands.RunAsync($"touch {containerMountPath}/should-fail.txt");
            var stat = await roSandbox.Commands.RunAsync(
                $"test ! -e {containerMountPath}/should-fail.txt && echo OK");
            var writeWasRejected = write.Error is not null || write.Logs.Stderr.Count > 0;
            var fileWasNotCreated =
                stat.Error is null &&
                stat.Logs.Stdout.Count == 1 &&
                stat.Logs.Stdout[0].Text == "OK";
            Assert.True(
                writeWasRejected || fileWasNotCreated,
                "Write on read-only PVC volume should fail or leave no created file.");
        }
        finally
        {
            try
            {
                await roSandbox.KillAsync();
            }
            catch
            {
            }

            await roSandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Sandbox_Create_With_PvcVolumeMount_SubPath()
    {
        var pvcVolumeName = "opensandbox-e2e-pvc-test";
        var containerMountPath = "/mnt/train";
        var subPathSandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _fixture.ConnectionConfig,
            Image = _fixture.DefaultImage,
            TimeoutSeconds = _fixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _fixture.DefaultReadyTimeoutSeconds,
            Volumes = new[]
            {
                new Volume
                {
                    Name = "test-pvc-subpath",
                    Pvc = new PVC { ClaimName = pvcVolumeName },
                    MountPath = containerMountPath,
                    ReadOnly = false,
                    SubPath = "datasets/train"
                }
            }
        });

        try
        {
            // Retry: bind mount propagation can sometimes lag on first access
            var marker = await RunWithRetryAsync(subPathSandbox, $"cat {containerMountPath}/marker.txt");
            Assert.Null(marker.Error);
            Assert.Single(marker.Logs.Stdout);
            Assert.Equal("pvc-subpath-marker", marker.Logs.Stdout[0].Text);

            var ls = await subPathSandbox.Commands.RunAsync($"ls {containerMountPath}/");
            Assert.Null(ls.Error);
            var lsText = string.Join("\n", ls.Logs.Stdout.Select(x => x.Text));
            Assert.Contains("marker.txt", lsText, StringComparison.Ordinal);
            Assert.DoesNotContain("datasets", lsText, StringComparison.Ordinal);

            var write = await subPathSandbox.Commands.RunAsync(
                $"echo 'subpath-write-test' > {containerMountPath}/output.txt");
            Assert.Null(write.Error);

            // Retry: bind mount propagation can sometimes lag on first access
            var readBack = await RunWithRetryAsync(subPathSandbox, $"cat {containerMountPath}/output.txt");
            Assert.Null(readBack.Error);
            Assert.Single(readBack.Logs.Stdout);
            Assert.Equal("subpath-write-test", readBack.Logs.Stdout[0].Text);
        }
        finally
        {
            try
            {
                await subPathSandbox.KillAsync();
            }
            catch
            {
            }

            await subPathSandbox.DisposeAsync();
        }
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Command_Execution_Success_WorkingDirectory_Background_Failure()
    {
        var sandbox = _fixture.Sandbox;

        var stdoutMessages = new ConcurrentBag<OutputMessage>();
        var stderrMessages = new ConcurrentBag<OutputMessage>();
        var results = new ConcurrentBag<ExecutionResult>();
        var errors = new ConcurrentBag<ExecutionError>();
        var completedEvents = new ConcurrentBag<ExecutionComplete>();
        var initEvents = new ConcurrentBag<ExecutionInit>();

        var handlers = new ExecutionHandlers
        {
            OnStdout = msg => { stdoutMessages.Add(msg); return Task.CompletedTask; },
            OnStderr = msg => { stderrMessages.Add(msg); return Task.CompletedTask; },
            OnResult = res => { results.Add(res); return Task.CompletedTask; },
            OnExecutionComplete = complete => { completedEvents.Add(complete); return Task.CompletedTask; },
            OnError = err => { errors.Add(err); return Task.CompletedTask; },
            OnInit = init => { initEvents.Add(init); return Task.CompletedTask; }
        };

        var echoResult = await sandbox.Commands.RunAsync("echo Hello OpenSandbox E2E", handlers: handlers);
        Assert.False(string.IsNullOrWhiteSpace(echoResult.Id));
        Assert.Null(echoResult.Error);
        Assert.Single(echoResult.Logs.Stdout);
        Assert.Equal("Hello OpenSandbox E2E", echoResult.Logs.Stdout[0].Text);
        AssertRecentTimestampMs(echoResult.Logs.Stdout[0].Timestamp, 60_000);
        Assert.Equal(0, echoResult.ExitCode);
        Assert.NotNull(echoResult.Complete);
        Assert.True(echoResult.Complete!.ExecutionTimeMs >= 0);
        AssertTerminalEventContract(initEvents, completedEvents, errors, echoResult.Id!);

        var pwdResult = await sandbox.Commands.RunAsync(
            "pwd",
            options: new RunCommandOptions { WorkingDirectory = "/tmp" });
        Assert.Null(pwdResult.Error);
        Assert.Single(pwdResult.Logs.Stdout);
        Assert.Equal("/tmp", pwdResult.Logs.Stdout[0].Text);
        Assert.Equal(0, pwdResult.ExitCode);
        Assert.NotNull(pwdResult.Complete);

        var start = DateTime.UtcNow;
        var backgroundResult = await sandbox.Commands.RunAsync(
            "sleep 30",
            options: new RunCommandOptions { Background = true });
        var elapsed = DateTime.UtcNow - start;
        Assert.True(elapsed.TotalSeconds < 10, "Background command should return quickly.");
        Assert.Null(backgroundResult.ExitCode);

        stdoutMessages = new ConcurrentBag<OutputMessage>();
        stderrMessages = new ConcurrentBag<OutputMessage>();
        errors = new ConcurrentBag<ExecutionError>();
        completedEvents = new ConcurrentBag<ExecutionComplete>();
        initEvents = new ConcurrentBag<ExecutionInit>();

        var failResult = await sandbox.Commands.RunAsync(
            "nonexistent-command-that-does-not-exist",
            handlers: new ExecutionHandlers
            {
                OnStdout = msg => { stdoutMessages.Add(msg); return Task.CompletedTask; },
                OnStderr = msg => { stderrMessages.Add(msg); return Task.CompletedTask; },
                OnError = err => { errors.Add(err); return Task.CompletedTask; },
                OnExecutionComplete = complete => { completedEvents.Add(complete); return Task.CompletedTask; },
                OnInit = init => { initEvents.Add(init); return Task.CompletedTask; }
            });

        Assert.NotNull(failResult.Error);
        Assert.Equal("CommandExecError", failResult.Error!.Name);
        Assert.True(failResult.Logs.Stderr.Count > 0);
        Assert.Contains(
            failResult.Logs.Stderr,
            msg => msg.Text.Contains("nonexistent-command-that-does-not-exist", StringComparison.Ordinal));
        Assert.Null(failResult.Complete);
        Assert.NotNull(failResult.ExitCode);
        Assert.Equal(int.Parse(failResult.Error.Value), failResult.ExitCode);
        AssertTerminalEventContract(initEvents, completedEvents, errors, failResult.Id!);
        Assert.Empty(completedEvents);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Command_Status_And_Background_Logs()
    {
        var sandbox = _fixture.Sandbox;

        var execResult = await sandbox.Commands.RunAsync(
            "sh -c 'echo log-line-1; echo log-line-2; sleep 2'",
            options: new RunCommandOptions { Background = true });
        Assert.False(string.IsNullOrWhiteSpace(execResult.Id));
        var commandId = execResult.Id!;

        var status = await sandbox.Commands.GetCommandStatusAsync(commandId);
        Assert.Equal(commandId, status.Id);
        Assert.NotNull(status.Running);

        var logsText = new StringBuilder();
        long? cursor = null;
        for (var i = 0; i < 20; i++)
        {
            var logs = await sandbox.Commands.GetBackgroundCommandLogsAsync(commandId, cursor);
            logsText.Append(logs.Content);
            cursor = logs.Cursor ?? cursor;
            if (logsText.ToString().Contains("log-line-2", StringComparison.Ordinal))
            {
                break;
            }

            await Task.Delay(1000);
        }

        var finalLogs = logsText.ToString();
        Assert.Contains("log-line-1", finalLogs, StringComparison.Ordinal);
        Assert.Contains("log-line-2", finalLogs, StringComparison.Ordinal);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Command_Env_Injection()
    {
        var sandbox = _fixture.Sandbox;
        var envKey = "OPEN_SANDBOX_E2E_CMD_ENV";
        var envValue = $"env-ok-{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        var probeCommand =
            $"sh -c 'if [ -z \"${{{envKey}:-}}\" ]; then echo \"__EMPTY__\"; else echo \"${{{envKey}}}\"; fi'";

        var baseline = await sandbox.Commands.RunAsync(probeCommand);
        Assert.Null(baseline.Error);
        var baselineOutput = string.Join("\n", baseline.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("__EMPTY__", baselineOutput);

        var injected = await sandbox.Commands.RunAsync(
            probeCommand,
            options: new RunCommandOptions
            {
                Envs = new Dictionary<string, string>
                {
                    [envKey] = envValue,
                    ["OPEN_SANDBOX_E2E_SECOND_ENV"] = "second-ok"
                }
            });
        Assert.Null(injected.Error);
        var injectedOutput = string.Join("\n", injected.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal(envValue, injectedOutput);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Bash_Session_API_WorkingDirectory_And_Env_Persistence()
    {
        var sandbox = _fixture.Sandbox;

        var sid = await sandbox.Commands.CreateSessionAsync(new CreateSessionOptions { WorkingDirectory = "/tmp" });
        Assert.False(string.IsNullOrWhiteSpace(sid));

        var run = await sandbox.Commands.RunInSessionAsync(sid, "pwd");
        Assert.Null(run.Error);
        Assert.Equal(0, run.ExitCode);
        var stdout = string.Join("", run.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("/tmp", stdout);

        run = await sandbox.Commands.RunInSessionAsync(
            sid,
            "pwd",
            options: new RunInSessionOptions { WorkingDirectory = "/var" });
        Assert.Null(run.Error);
        Assert.Equal(0, run.ExitCode);
        stdout = string.Join("", run.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("/var", stdout);

        run = await sandbox.Commands.RunInSessionAsync(
            sid,
            "pwd",
            options: new RunInSessionOptions { WorkingDirectory = "/tmp" });
        Assert.Null(run.Error);
        Assert.Equal(0, run.ExitCode);
        stdout = string.Join("", run.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("/tmp", stdout);

        run = await sandbox.Commands.RunInSessionAsync(sid, "export E2E_SESSION_ENV=session-env-ok");
        Assert.Null(run.Error);

        run = await sandbox.Commands.RunInSessionAsync(sid, "echo $E2E_SESSION_ENV");
        Assert.Null(run.Error);
        Assert.Equal(0, run.ExitCode);
        stdout = string.Join("", run.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("session-env-ok", stdout);

        run = await sandbox.Commands.RunInSessionAsync(sid, "sh -c 'echo session-fail >&2; exit 7'");
        Assert.NotNull(run.Error);
        Assert.Equal("CommandExecError", run.Error!.Name);
        Assert.Equal("7", run.Error.Value);
        Assert.Equal(7, run.ExitCode);
        Assert.Null(run.Complete);

        var sid2 = await sandbox.Commands.CreateSessionAsync(new CreateSessionOptions { WorkingDirectory = "/var" });
        Assert.False(string.IsNullOrWhiteSpace(sid2));
        run = await sandbox.Commands.RunInSessionAsync(sid2, "pwd");
        Assert.Null(run.Error);
        Assert.Equal(0, run.ExitCode);
        stdout = string.Join("", run.Logs.Stdout.Select(m => m.Text)).Trim();
        Assert.Equal("/var", stdout);

        await sandbox.Commands.DeleteSessionAsync(sid);
        await sandbox.Commands.DeleteSessionAsync(sid2);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Filesystem_Operations_CRUD_Replace_Move_Delete()
    {
        var sandbox = _fixture.Sandbox;

        var testDir1 = $"/tmp/fs_test1_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        var testDir2 = $"/tmp/fs_test2_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";

        await sandbox.Files.CreateDirectoriesAsync(new[]
        {
            new CreateDirectoryEntry { Path = testDir1, Mode = 755 },
            new CreateDirectoryEntry { Path = testDir2, Mode = 644 }
        });

        var dirInfo = await sandbox.Files.GetFileInfoAsync(new[] { testDir1, testDir2 });
        Assert.Equal(testDir1, dirInfo[testDir1].Path);
        Assert.Equal(755, dirInfo[testDir1].Mode);
        AssertTimesClose(dirInfo[testDir1].CreatedAt, dirInfo[testDir1].ModifiedAt, 2);

        var testFile1 = $"{testDir1}/test_file1.txt";
        var testFile2 = $"{testDir1}/test_file2.txt";
        var testFile3 = $"{testDir1}/test_file3.txt";
        var testContent = "Hello Filesystem! Line 2. Line 3.";

        await sandbox.Files.WriteFilesAsync(new[]
        {
            new WriteEntry { Path = testFile1, Data = testContent, Mode = 644 },
            new WriteEntry { Path = testFile2, Data = Encoding.UTF8.GetBytes(testContent), Mode = 755 },
            new WriteEntry { Path = testFile3, Data = new MemoryStream(Encoding.UTF8.GetBytes(testContent)), Mode = 755 }
        });

        var readContent1 = await sandbox.Files.ReadFileAsync(
            testFile1,
            new ReadFileOptions { Encoding = "utf-8" });
        var readContent1Partial = await sandbox.Files.ReadFileAsync(
            testFile1,
            new ReadFileOptions { Encoding = "utf-8", Range = "bytes=0-9" });
        var readBytes2 = await sandbox.Files.ReadBytesAsync(testFile2);
        var readContent2 = Encoding.UTF8.GetString(readBytes2);

        var chunks = new List<byte>();
        await foreach (var chunk in sandbox.Files.ReadBytesStreamAsync(testFile3))
        {
            chunks.AddRange(chunk);
        }

        var readContent3 = Encoding.UTF8.GetString(chunks.ToArray());

        Assert.Equal(testContent, readContent1);
        Assert.Equal(testContent, readContent2);
        Assert.Equal(testContent, readContent3);
        Assert.Equal(testContent.Substring(0, 10), readContent1Partial);

        var fileInfoMap = await sandbox.Files.GetFileInfoAsync(new[] { testFile1, testFile2, testFile3 });
        var expectedSize = Encoding.UTF8.GetBytes(testContent).Length;
        Assert.Equal(expectedSize, fileInfoMap[testFile1].Size);
        Assert.Equal(expectedSize, fileInfoMap[testFile2].Size);
        Assert.Equal(expectedSize, fileInfoMap[testFile3].Size);
        AssertTimesClose(fileInfoMap[testFile1].CreatedAt, fileInfoMap[testFile1].ModifiedAt, 2);

        var found = new HashSet<string>();
        var searchResults = await sandbox.Files.SearchAsync(new SearchEntry { Path = testDir1, Pattern = "*" });
        foreach (var entry in searchResults)
        {
            found.Add(entry.Path);
        }
        Assert.Equal(new HashSet<string> { testFile1, testFile2, testFile3 }, found);

        await sandbox.Files.SetPermissionsAsync(new[]
        {
            new SetPermissionEntry { Path = testFile1, Mode = 755 },
            new SetPermissionEntry { Path = testFile2, Mode = 600 }
        });

        var updatedInfo = await sandbox.Files.GetFileInfoAsync(new[] { testFile1, testFile2 });
        Assert.Equal(755, updatedInfo[testFile1].Mode);
        Assert.Equal(600, updatedInfo[testFile2].Mode);

        var beforeUpdate = (await sandbox.Files.GetFileInfoAsync(new[] { testFile1 }))[testFile1];
        var updatedContent1 = testContent + " Appended line.";
        await Task.Delay(50);
        await sandbox.Files.WriteFilesAsync(new[]
        {
            new WriteEntry { Path = testFile1, Data = updatedContent1, Mode = 644 }
        });

        var newContent1 = await sandbox.Files.ReadFileAsync(testFile1, new ReadFileOptions { Encoding = "utf-8" });
        Assert.Equal(updatedContent1, newContent1);
        var afterUpdate = (await sandbox.Files.GetFileInfoAsync(new[] { testFile1 }))[testFile1];
        AssertModifiedUpdated(beforeUpdate.ModifiedAt, afterUpdate.ModifiedAt, 1, 1000);

        await Task.Delay(50);
        await sandbox.Files.ReplaceContentsAsync(new[]
        {
            new ContentReplaceEntry
            {
                Path = testFile1,
                OldContent = "Appended line.",
                NewContent = "Replaced line."
            }
        });

        var replaced = await sandbox.Files.ReadFileAsync(testFile1, new ReadFileOptions { Encoding = "utf-8" });
        Assert.Contains("Replaced line.", replaced, StringComparison.Ordinal);
        Assert.DoesNotContain("Appended line.", replaced, StringComparison.Ordinal);

        var movedPath = $"{testDir2}/moved_file3.txt";
        await sandbox.Files.MoveFilesAsync(new[] { new MoveEntry { Src = testFile3, Dest = movedPath } });
        var movedBytes = await sandbox.Files.ReadBytesAsync(movedPath);
        Assert.Equal(testContent, Encoding.UTF8.GetString(movedBytes));
        await Assert.ThrowsAnyAsync<Exception>(() => sandbox.Files.ReadBytesAsync(testFile3));

        await sandbox.Files.DeleteFilesAsync(new[] { testFile2 });
        await Assert.ThrowsAnyAsync<Exception>(() => sandbox.Files.ReadFileAsync(testFile2));

        await sandbox.Files.DeleteDirectoriesAsync(new[] { testDir1, testDir2 });
        var verify = await sandbox.Commands.RunAsync(
            $"test ! -d {testDir1} && test ! -d {testDir2} && echo OK",
            options: new RunCommandOptions { WorkingDirectory = "/tmp" });
        for (var attempt = 0; attempt < 3; attempt++)
        {
            var verified =
                verify.Error is null &&
                verify.Logs.Stdout.Count == 1 &&
                verify.Logs.Stdout[0].Text == "OK";
            if (verified)
            {
                break;
            }

            await Task.Delay(1000);
            verify = await sandbox.Commands.RunAsync(
                $"test ! -d {testDir1} && test ! -d {testDir2} && echo OK",
                options: new RunCommandOptions { WorkingDirectory = "/tmp" });
        }
        Assert.Null(verify.Error);
        Assert.Single(verify.Logs.Stdout);
        Assert.Equal("OK", verify.Logs.Stdout[0].Text);
    }

    [Fact(Timeout = 2 * 60 * 1000)]
    public async Task Command_Interrupt()
    {
        var sandbox = _fixture.Sandbox;

        var initEvents = new ConcurrentBag<ExecutionInit>();
        var completedEvents = new ConcurrentBag<ExecutionComplete>();
        var errors = new ConcurrentBag<ExecutionError>();
        var initLatch = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);

        var handlers = new ExecutionHandlers
        {
            OnInit = init =>
            {
                initEvents.Add(init);
                initLatch.TrySetResult(init.Id);
                return Task.CompletedTask;
            },
            OnExecutionComplete = complete => { completedEvents.Add(complete); return Task.CompletedTask; },
            OnError = err => { errors.Add(err); return Task.CompletedTask; }
        };

        var executionTask = sandbox.Commands.RunAsync("sleep 30", handlers: handlers);
        var id = await initLatch.Task.WaitAsync(TimeSpan.FromSeconds(15));

        await Task.Delay(2000);
        await sandbox.Commands.InterruptAsync(id);

        var result = await executionTask.WaitAsync(TimeSpan.FromSeconds(30));
        Assert.Equal(id, result.Id);
        Assert.True((completedEvents.Count > 0) ^ (errors.Count > 0));
        Assert.True(result.Error != null || result.Logs.Stderr.Count > 0);
    }

    [Fact(Timeout = 5 * 60 * 1000)]
    public async Task Sandbox_Pause_And_Resume()
    {
        return; // skip pause/resume e2e test

        var sandbox = _fixture.Sandbox;

        await Task.Delay(5000);
        await sandbox.PauseAsync();

        var pausedInfo = await WaitForStateAsync(sandbox, SandboxStates.Paused, TimeSpan.FromMinutes(5));
        Assert.Equal(SandboxStates.Paused, pausedInfo.Status.State);

        var healthy = true;
        for (var i = 0; i < 10; i++)
        {
            healthy = await sandbox.IsHealthyAsync();
            if (!healthy)
            {
                break;
            }
            await Task.Delay(500);
        }
        Assert.False(healthy, "Sandbox should be unhealthy after pause.");

        var resumed = await sandbox.ResumeAsync(new SandboxResumeOptions
        {
            ReadyTimeoutSeconds = 60,
            HealthCheckPollingInterval = 1000
        });

        var resumedInfo = await WaitForStateAsync(resumed, SandboxStates.Running, TimeSpan.FromMinutes(3));
        Assert.Equal(SandboxStates.Running, resumedInfo.Status.State);

        var isHealthy = false;
        for (var i = 0; i < 30; i++)
        {
            isHealthy = await resumed.IsHealthyAsync();
            if (isHealthy)
            {
                break;
            }
            await Task.Delay(1000);
        }
        Assert.True(isHealthy, "Sandbox should be healthy after resume.");

        // Smoke-check command path after resume to ensure execd adapter is usable.
        var echo = await resumed.Commands.RunAsync("echo resume-ok");
        Assert.Null(echo.Error);
        Assert.Single(echo.Logs.Stdout);
        Assert.Equal("resume-ok", echo.Logs.Stdout[0].Text);
    }

    private static void AssertRecentTimestampMs(long ts, long toleranceMs)
    {
        Assert.True(ts > 0);
        var delta = Math.Abs(DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() - ts);
        Assert.True(delta <= toleranceMs, $"timestamp too far from now: delta={delta}ms (ts={ts})");
    }

    private static void AssertEndpointHasPort(string endpoint, int expectedPort)
    {
        Assert.False(endpoint.Contains("://", StringComparison.Ordinal), $"unexpected scheme in endpoint: {endpoint}");
        if (endpoint.Contains('/'))
        {
            Assert.EndsWith($"/{expectedPort}", endpoint, StringComparison.Ordinal);
            Assert.False(string.IsNullOrWhiteSpace(endpoint.Split('/', 2)[0]));
            return;
        }

        var parts = endpoint.Split(':');
        Assert.True(parts.Length >= 2, $"missing host:port in endpoint: {endpoint}");
        var port = parts[^1];
        Assert.True(int.TryParse(port, out var parsed));
        Assert.Equal(expectedPort, parsed);
    }

    private static void AssertTimesClose(DateTime? createdAt, DateTime? modifiedAt, double toleranceSeconds)
    {
        Assert.NotNull(createdAt);
        Assert.NotNull(modifiedAt);
        var delta = Math.Abs((modifiedAt!.Value - createdAt!.Value).TotalSeconds);
        Assert.True(delta <= toleranceSeconds, $"created/modified skew too large: {delta}s");
    }

    private static void AssertModifiedUpdated(DateTime? before, DateTime? after, int minDeltaMs, int allowSkewMs)
    {
        Assert.NotNull(before);
        Assert.NotNull(after);
        var deltaMs = (after!.Value - before!.Value).TotalMilliseconds;
        Assert.True(deltaMs >= minDeltaMs - allowSkewMs, $"modified_at did not update as expected: delta_ms={deltaMs}");
    }

    private static void AssertTerminalEventContract(
        IEnumerable<ExecutionInit> initEvents,
        IEnumerable<ExecutionComplete> completedEvents,
        IEnumerable<ExecutionError> errors,
        string executionId)
    {
        var initList = initEvents.ToList();
        var completeList = completedEvents.ToList();
        var errorList = errors.ToList();

        Assert.Single(initList);
        Assert.False(string.IsNullOrWhiteSpace(initList[0].Id));
        Assert.Equal(executionId, initList[0].Id);
        AssertRecentTimestampMs(initList[0].Timestamp, 120_000);

        var hasComplete = completeList.Count > 0;
        var hasError = errorList.Count > 0;
        Assert.True(hasComplete || hasError);

        if (hasComplete)
        {
            Assert.Single(completeList);
            AssertRecentTimestampMs(completeList[0].Timestamp, 180_000);
            Assert.True(completeList[0].ExecutionTimeMs >= 0);
        }

        if (hasError)
        {
            Assert.False(string.IsNullOrWhiteSpace(errorList[0].Name));
            Assert.False(string.IsNullOrWhiteSpace(errorList[0].Value));
            AssertRecentTimestampMs(errorList[0].Timestamp, 180_000);
        }
    }

    private static async Task<SandboxInfo> WaitForStateAsync(
        Sandbox sandbox,
        string expectedState,
        TimeSpan timeout)
    {
        var deadline = DateTime.UtcNow + timeout;
        SandboxInfo info;
        while (true)
        {
            info = await sandbox.GetInfoAsync();
            if (info.Status.State == expectedState)
            {
                return info;
            }

            if (DateTime.UtcNow > deadline)
            {
                throw new TimeoutException($"Timed out waiting for state={expectedState}, last_state={info.Status.State}");
            }

            await Task.Delay(1000);
        }
    }

    private static async Task<Execution> RunWithRetryAsync(Sandbox sandbox, string command, int maxAttempts = 5, int delayMs = 500)
    {
        Execution? result = null;
        for (int attempt = 0; attempt < maxAttempts; attempt++)
        {
            result = await sandbox.Commands.RunAsync(command);
            if (result.Error == null && result.Logs.Stdout.Count > 0)
                return result;
            if (attempt < maxAttempts - 1)
                await Task.Delay(delayMs);
        }
        return result!;
    }

    /// <summary>
    /// Polls curl against <paramref name="url"/> until the egress sidecar blocks
    /// it (Execution.Error becomes non-null), or the timeout elapses. NetworkPolicy
    /// sidecars sometimes accept connections before iptables/proxy rules apply,
    /// so a fixed sleep is flaky.
    /// </summary>
    private static async Task WaitUntilEgressBlocksAsync(Sandbox sandbox, string url, TimeSpan timeout)
    {
        var deadline = DateTime.UtcNow + timeout;
        Execution? last = null;
        while (DateTime.UtcNow < deadline)
        {
            try
            {
                last = await sandbox.Commands.RunAsync($"curl -I {url}");
                if (last?.Error != null)
                {
                    return;
                }
            }
            catch
            {
                // Transient SDK/SSE errors during sidecar warmup — keep polling.
            }
            await Task.Delay(500);
        }
        Assert.Fail($"Egress policy did not block {url} within {timeout} (last error={last?.Error?.ToString() ?? "null"})");
    }
}

public sealed class SandboxE2ETestFixture : IAsyncLifetime
{
    private readonly E2ETestFixture _baseFixture = new();
    private Sandbox? _sandbox;

    public ConnectionConfig ConnectionConfig => _baseFixture.ConnectionConfig;
    public ConnectionConfig ServerProxyConnectionConfig => _baseFixture.ServerProxyConnectionConfig;
    public string DefaultImage => _baseFixture.DefaultImage;
    public int DefaultTimeoutSeconds => _baseFixture.DefaultTimeoutSeconds;
    public int DefaultReadyTimeoutSeconds => _baseFixture.DefaultReadyTimeoutSeconds;
    public Sandbox Sandbox => _sandbox ?? throw new InvalidOperationException("Sandbox is not initialized.");

    public async Task InitializeAsync()
    {
        _sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            ConnectionConfig = _baseFixture.ConnectionConfig,
            Image = _baseFixture.DefaultImage,
            TimeoutSeconds = _baseFixture.DefaultTimeoutSeconds,
            ReadyTimeoutSeconds = _baseFixture.DefaultReadyTimeoutSeconds,
            Metadata = new Dictionary<string, string> { ["tag"] = "csharp-e2e-test" },
            Env = new Dictionary<string, string> { ["E2E_TEST"] = "true", ["EXECD_API_GRACE_SHUTDOWN"] = "3s", ["EXECD_JUPYTER_IDLE_POLL_INTERVAL"] = "200ms" },
            HealthCheckPollingInterval = 500
        });
    }

    public async Task DisposeAsync()
    {
        if (_sandbox == null)
        {
            return;
        }

        try
        {
            await _sandbox.KillAsync();
        }
        catch
        {
        }

        await _sandbox.DisposeAsync();
    }
}
