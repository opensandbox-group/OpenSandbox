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
using OpenSandbox.Core;
using OpenSandbox.Models;
using Xunit;
using Xunit.Abstractions;

namespace OpenSandbox.E2ETests;

[Collection("CSharp E2E Tests")]
public sealed class IsolatedSessionE2ETests : IAsyncLifetime
{
    private readonly E2ETestFixture _fixture;
    private readonly ITestOutputHelper _output;
    private Sandbox? _sandbox;

    public IsolatedSessionE2ETests(E2ETestFixture fixture, ITestOutputHelper output)
    {
        _fixture = fixture;
        _output = output;
    }

    private static string StdoutText(Execution exec)
        => string.Join("", exec.Logs.Stdout.Select(m => m.Text));

    public async Task InitializeAsync()
    {
        _sandbox = await Sandbox.CreateAsync(new SandboxCreateOptions
        {
            Image = _fixture.DefaultImage,
            ConnectionConfig = _fixture.ConnectionConfig,
            Extensions = new Dictionary<string, string>
            {
                ["bootstrap.execd.isolation"] = "enable"
            }
        });

        var caps = await _sandbox.Isolation.CapabilitiesAsync();
        _output.WriteLine(
            $"Isolation capabilities: available={caps.Available} isolator={caps.Isolator} " +
            $"version={caps.Version} message={caps.Message}");
        Assert.True(caps.Available, $"Isolation NOT available: {caps.Message ?? "unknown reason"}");
    }

    public async Task DisposeAsync()
    {
        if (_sandbox != null)
        {
            await _sandbox.KillAsync();
            await _sandbox.DisposeAsync();
        }
    }

    [Fact]
    public async Task TestCapabilities()
    {
        var caps = await _sandbox!.Isolation.CapabilitiesAsync();
        Assert.True(caps.Available);
    }

    [Fact]
    public async Task TestSessionLifecycle()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        Assert.NotEmpty(session.SessionId);

        var state = await session.GetAsync();
        Assert.Equal("active", state.Status);

        await session.DeleteAsync();
    }

    [Fact]
    public async Task TestRunEcho()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var exec = await session.RunAsync("echo hello-isolation");
            Assert.Contains("hello-isolation", StdoutText(exec));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestPidIsolation()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var exec = await session.RunAsync("echo $$");
            var pid = int.Parse(StdoutText(exec).Trim());
            Assert.True(pid <= 2, $"expected PID 1 or 2, got {pid}");
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRunWithEnvs()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var exec = await session.RunAsync(
                "echo $MY_VAR",
                new IsolatedRunOpts(new Dictionary<string, string> { ["MY_VAR"] = "test-value-42" }));
            Assert.Contains("test-value-42", StdoutText(exec));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestSessionStatePersists()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            await session.RunAsync("export PERSIST_VAR=abc123");
            var exec = await session.RunAsync("echo $PERSIST_VAR");
            Assert.Contains("abc123", StdoutText(exec));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestTmpIsolation()
    {
        await _sandbox!.Commands.RunAsync("mkdir -p /workspace");

        var sessionA = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/workspace", "rw"), "strict"));
        var sessionB = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/workspace", "rw"), "strict"));
        try
        {
            await sessionA.RunAsync("echo secret > /tmp/isolated_test_file.txt");
            var exec = await sessionB.RunAsync(
                "cat /tmp/isolated_test_file.txt 2>&1 || echo NOT_FOUND");
            Assert.True(
                StdoutText(exec).Contains("NOT_FOUND") || StdoutText(exec).Contains("No such file"),
                $"expected /tmp isolation, got: {StdoutText(exec)}");
        }
        finally
        {
            await sessionA.DeleteAsync();
            await sessionB.DeleteAsync();
        }
    }

    // ── RW mode: filesystem API tests ───────────────────────────────

    [Fact]
    public async Task TestRwFilesUploadDownload()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var path = $"/tmp/upload_rw_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "rw upload", Mode = 644 }
            });
            var content = await session.Files.ReadFileAsync(path);
            Assert.Equal("rw upload", content);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesInfo()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var path = "/tmp/info_rw.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "info", Mode = 644 }
            });
            var infoMap = await session.Files.GetFileInfoAsync(new[] { path });
            Assert.True(infoMap.ContainsKey(path));
            Assert.Equal(4, infoMap[path].Size);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesSearch()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var prefix = $"/tmp/search_rw_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
            await session.RunAsync($"mkdir -p {prefix}");
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = $"{prefix}/a.txt", Data = "a", Mode = 644 },
                new WriteEntry { Path = $"{prefix}/b.txt", Data = "b", Mode = 644 },
                new WriteEntry { Path = $"{prefix}/c.log", Data = "c", Mode = 644 },
            });
            var results = await session.Files.SearchAsync(
                new SearchEntry { Path = prefix, Pattern = "*.txt" });
            Assert.Equal(2, results.Count);
            var paths = results.Select(r => r.Path).ToList();
            Assert.Contains(paths, p => p.Contains("a.txt"));
            Assert.Contains(paths, p => p.Contains("b.txt"));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesMkdir()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var d = $"/tmp/mkdir_rw_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
            await session.Files.CreateDirectoriesAsync(new[]
            {
                new CreateDirectoryEntry { Path = d, Mode = 755 }
            });
            var infoMap = await session.Files.GetFileInfoAsync(new[] { d });
            Assert.True(infoMap.ContainsKey(d));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesDelete()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var path = "/tmp/delete_rw.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "del", Mode = 644 }
            });
            await session.Files.DeleteFilesAsync(new[] { path });
            await Assert.ThrowsAsync<SandboxApiException>(
                () => session.Files.GetFileInfoAsync(new[] { path }));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesMove()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var src = "/tmp/mv_rw_src.txt";
            var dst = "/tmp/mv_rw_dst.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = src, Data = "move", Mode = 644 }
            });
            await session.Files.MoveFilesAsync(new[]
            {
                new MoveEntry { Src = src, Dest = dst }
            });
            var content = await session.Files.ReadFileAsync(dst);
            Assert.Equal("move", content);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesChmod()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var path = "/tmp/chmod_rw.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "ch", Mode = 644 }
            });
            await session.Files.SetPermissionsAsync(new[]
            {
                new SetPermissionEntry { Path = path, Mode = 755 }
            });
            var infoMap = await session.Files.GetFileInfoAsync(new[] { path });
            Assert.Equal(755, infoMap[path].Mode);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesReplace()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var path = "/tmp/replace_rw.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "hello old world", Mode = 644 }
            });
            await session.Files.ReplaceContentsAsync(new[]
            {
                new ContentReplaceEntry { Path = path, OldContent = "old", NewContent = "new" }
            });
            var content = await session.Files.ReadFileAsync(path);
            Assert.Contains("new", content);
            Assert.DoesNotContain("old", content);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwFilesListDirectory()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            var prefix = $"/tmp/listdir_rw_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
            await session.RunAsync($"mkdir -p {prefix}/sub");
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = $"{prefix}/f1.txt", Data = "f1", Mode = 644 },
                new WriteEntry { Path = $"{prefix}/sub/f2.txt", Data = "f2", Mode = 644 },
            });
            var entries = await session.Files.ListDirectoryAsync(prefix, depth: 1);
            var names = entries.Select(e => e.Path).ToList();
            Assert.Contains(names, n => n.Contains("f1.txt"));
            Assert.Contains(names, n => n.Contains("sub"));
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRwHostVisible()
    {
        var marker = $"rw_visible_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")));
        try
        {
            await session.RunAsync($"echo rw-data > /tmp/{marker}");
            var hostCheck = await _sandbox.Commands.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("rw-data", StdoutText(hostCheck));
        }
        finally
        {
            await session.RunAsync($"rm -f /tmp/{marker}");
            await session.DeleteAsync();
        }
    }

    // ── RO mode tests ───────────────────────────────────────────────

    [Fact]
    public async Task TestRoCanReadExistingFiles()
    {
        var marker = $"ro_read_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"echo ro-data > /tmp/{marker}");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "ro")));
        try
        {
            var exec = await session.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("ro-data", StdoutText(exec));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestRoCannotWrite()
    {
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "ro")));
        try
        {
            var exec = await session.RunAsync(
                "echo fail > /tmp/ro_write_test.txt 2>&1; echo EXIT=$?");
            var text = StdoutText(exec);
            Assert.True(
                text.Contains("EXIT=1") || text.Contains("Read-only") || text.Contains("Permission denied"),
                $"expected RO rejection, got: {text}");
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestRoFilesApiRead()
    {
        var marker = $"ro_api_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"echo ro-api-data > /tmp/{marker}");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "ro")));
        try
        {
            var content = await session.Files.ReadFileAsync($"/tmp/{marker}");
            Assert.Contains("ro-api-data", content);
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestRoFilesApiSearch()
    {
        var prefix = $"/tmp/ro_search_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        await _sandbox!.Commands.RunAsync($"mkdir -p {prefix} && echo x > {prefix}/file.txt");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "ro")));
        try
        {
            var results = await session.Files.SearchAsync(
                new SearchEntry { Path = prefix, Pattern = "*.txt" });
            Assert.True(results.Count >= 1);
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {prefix}");
        }
    }

    [Fact]
    public async Task TestRoFilesApiListDirectory()
    {
        var prefix = $"/tmp/ro_listdir_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        await _sandbox!.Commands.RunAsync($"mkdir -p {prefix} && echo x > {prefix}/f.txt");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "ro")));
        try
        {
            var entries = await session.Files.ListDirectoryAsync(prefix, depth: 1);
            Assert.True(entries.Count >= 1);
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {prefix}");
        }
    }

    // ── Overlay mode tests ──────────────────────────────────────────

    private async Task<bool> OverlaySupported()
    {
        var caps = await _sandbox!.Isolation.CapabilitiesAsync();
        return caps.CommitSupported || caps.DiffSupported;
    }

    [Fact]
    public async Task TestOverlayWritesNotVisibleOnHost()
    {
        if (!await OverlaySupported()) return;

        var marker = $"overlay_invis_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.RunAsync($"echo overlay-data > /tmp/{marker}");
            var hostCheck = await _sandbox.Commands.RunAsync(
                $"cat /tmp/{marker} 2>&1 || echo NOT_FOUND");
            var text = StdoutText(hostCheck);
            Assert.True(
                text.Contains("NOT_FOUND") || text.Contains("No such file"),
                $"expected overlay isolation, got: {text}");
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestOverlayCanReadHostFiles()
    {
        if (!await OverlaySupported()) return;

        var marker = $"overlay_lower_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"echo lower-data > /tmp/{marker}");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            var exec = await session.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("lower-data", StdoutText(exec));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestOverlayCowDoesNotMutateHost()
    {
        if (!await OverlaySupported()) return;

        var marker = $"overlay_cow_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"echo original > /tmp/{marker}");
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.RunAsync($"echo modified > /tmp/{marker}");
            var inSession = await session.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("modified", StdoutText(inSession));
            var hostCheck = await _sandbox.Commands.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("original", StdoutText(hostCheck));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiUploadDownload()
    {
        if (!await OverlaySupported()) return;

        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            var path = $"/tmp/ov_upload_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = path, Data = "overlay file", Mode = 644 }
            });
            var content = await session.Files.ReadFileAsync(path);
            Assert.Equal("overlay file", content);
            // Host should NOT see it
            var hostCheck = await _sandbox.Commands.RunAsync(
                $"cat {path} 2>&1 || echo NOT_FOUND");
            var hostText = StdoutText(hostCheck);
            Assert.True(
                hostText.Contains("NOT_FOUND") || hostText.Contains("No such file"),
                $"expected file not visible on host, got: {hostText}");
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiSearch()
    {
        if (!await OverlaySupported()) return;

        var prefix = $"/tmp/ov_search_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        await _sandbox!.Commands.RunAsync($"mkdir -p {prefix} && echo lower > {prefix}/lower.txt");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = $"{prefix}/upper.txt", Data = "upper", Mode = 644 }
            });
            var results = await session.Files.SearchAsync(
                new SearchEntry { Path = prefix, Pattern = "*.txt" });
            var paths = results.Select(r => r.Path).ToList();
            Assert.Contains(paths, p => p.Contains("lower.txt"));
            Assert.Contains(paths, p => p.Contains("upper.txt"));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {prefix}");
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiDelete()
    {
        if (!await OverlaySupported()) return;

        var prefix = $"/tmp/ov_del_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        await _sandbox!.Commands.RunAsync($"mkdir -p {prefix} && echo x > {prefix}/target.txt");
        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.Files.DeleteFilesAsync(new[] { $"{prefix}/target.txt" });
            await Assert.ThrowsAsync<SandboxApiException>(
                () => session.Files.GetFileInfoAsync(new[] { $"{prefix}/target.txt" }));
            // Host file should be untouched
            var hostCheck = await _sandbox.Commands.RunAsync($"cat {prefix}/target.txt");
            Assert.Contains("x", StdoutText(hostCheck));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {prefix}");
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiMove()
    {
        if (!await OverlaySupported()) return;

        var session = await _sandbox!.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            var src = "/tmp/ov_mv_src.txt";
            var dst = "/tmp/ov_mv_dst.txt";
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = src, Data = "moveme", Mode = 644 }
            });
            await session.Files.MoveFilesAsync(new[]
            {
                new MoveEntry { Src = src, Dest = dst }
            });
            var content = await session.Files.ReadFileAsync(dst);
            Assert.Equal("moveme", content);
        }
        finally
        {
            await session.DeleteAsync();
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiChmod()
    {
        if (!await OverlaySupported()) return;

        var marker = $"ov_chmod_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"echo ch > /tmp/{marker} && chmod 644 /tmp/{marker}");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.Files.SetPermissionsAsync(new[]
            {
                new SetPermissionEntry { Path = $"/tmp/{marker}", Mode = 755 }
            });
            var infoMap = await session.Files.GetFileInfoAsync(new[] { $"/tmp/{marker}" });
            Assert.Equal(755, infoMap[$"/tmp/{marker}"].Mode);
            // Host should still be 644
            var hostCheck = await _sandbox.Commands.RunAsync($"stat -c %a /tmp/{marker}");
            Assert.Contains("644", StdoutText(hostCheck));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiReplace()
    {
        if (!await OverlaySupported()) return;

        var marker = $"ov_repl_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}.txt";
        await _sandbox!.Commands.RunAsync($"printf 'hello old world' > /tmp/{marker}");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.Files.ReplaceContentsAsync(new[]
            {
                new ContentReplaceEntry
                {
                    Path = $"/tmp/{marker}",
                    OldContent = "old",
                    NewContent = "new"
                }
            });
            var content = await session.Files.ReadFileAsync($"/tmp/{marker}");
            Assert.Contains("new", content);
            Assert.DoesNotContain("old", content);
            // Host unchanged
            var hostCheck = await _sandbox.Commands.RunAsync($"cat /tmp/{marker}");
            Assert.Contains("old", StdoutText(hostCheck));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -f /tmp/{marker}");
        }
    }

    [Fact]
    public async Task TestOverlayFilesApiListDirectory()
    {
        if (!await OverlaySupported()) return;

        var prefix = $"/tmp/ov_ls_{DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()}";
        await _sandbox!.Commands.RunAsync($"mkdir -p {prefix} && echo l > {prefix}/lower.txt");
        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "overlay")));
        try
        {
            await session.Files.WriteFilesAsync(new[]
            {
                new WriteEntry { Path = $"{prefix}/upper.txt", Data = "u", Mode = 644 }
            });
            var entries = await session.Files.ListDirectoryAsync(prefix, depth: 1);
            var names = entries.Select(e => e.Path).ToList();
            Assert.Contains(names, n => n.Contains("lower.txt"));
            Assert.Contains(names, n => n.Contains("upper.txt"));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {prefix}");
        }
    }

    // ── RunOnceAsync / WithSessionAsync convenience API tests ────────

    [Fact]
    public async Task TestRunOnce()
    {
        var exec = await _sandbox!.Isolation.RunOnceAsync(
            "echo runonce-e2e", "/tmp", workspaceMode: "rw");
        Assert.Contains("runonce-e2e", StdoutText(exec));
    }

    [Fact]
    public async Task TestRunOnceWithEnvs()
    {
        var exec = await _sandbox!.Isolation.RunOnceAsync(
            "echo $E2E_RUN_ONCE",
            "/tmp",
            workspaceMode: "rw",
            opts: new IsolatedRunOpts(
                new Dictionary<string, string> { ["E2E_RUN_ONCE"] = "cs-value" }));
        Assert.Contains("cs-value", StdoutText(exec));
    }

    [Fact]
    public async Task TestWithSession()
    {
        var output = await _sandbox!.Isolation.WithSessionAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")),
            async session =>
            {
                await session.RunAsync("export WS_VAR=with-session-cs");
                var exec = await session.RunAsync("echo $WS_VAR");
                return StdoutText(exec);
            });
        Assert.Contains("with-session-cs", output);
    }

    [Fact]
    public async Task TestWithSessionMultiRun()
    {
        var output = await _sandbox!.Isolation.WithSessionAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/tmp", "rw")),
            async session =>
            {
                await session.RunAsync("echo step1 > /tmp/ws_test_cs.txt");
                var exec = await session.RunAsync("cat /tmp/ws_test_cs.txt");
                return StdoutText(exec);
            });
        Assert.Contains("step1", output);
    }

    // ── Bind mount tests (explicit source->dest binds) ─────────────────

    [Fact]
    public async Task TestBindReadWriteHostVisible()
    {
        var ts = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        // Source must be within the execd writable allowlist (e.g. /data).
        var srcDir = $"/data/bind_rw_{ts}";
        var dest = "/mnt/bind_rw";
        var fileName = "from_sandbox.txt";
        var content = "bind-rw-visible-on-host";

        // Create the source dir and the destination mount point (bwrap binds
        // onto an existing dir; it cannot create one under the read-only root).
        await _sandbox!.Commands.RunAsync($"mkdir -p {srcDir} {dest}");

        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(
                new IsolatedWorkspaceSpec("/tmp", "rw"),
                Binds: new List<BindMount> { new BindMount(srcDir, dest) }));
        try
        {
            var exec = await session.RunAsync(
                $"echo -n {content} > {dest}/{fileName} && cat {dest}/{fileName}");
            Assert.Contains(content, StdoutText(exec));

            var hostCheck = await _sandbox.Commands.RunAsync($"cat {srcDir}/{fileName}");
            Assert.Contains(content, StdoutText(hostCheck));
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {srcDir}");
        }
    }

    [Fact]
    public async Task TestBindIllegalRejected()
    {
        await Assert.ThrowsAsync<SandboxApiException>(
            () => _sandbox!.Isolation.CreateAsync(
                new CreateIsolatedSessionRequest(
                    new IsolatedWorkspaceSpec("/tmp", "rw"),
                    // /etc is not in the writable allowlist.
                    Binds: new List<BindMount> { new BindMount("/etc", "/mnt/etc") })));
    }

    [Fact]
    public async Task TestBindReadOnlyReadable()
    {
        var ts = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        var srcDir = $"/data/bind_ro_{ts}";
        var dest = "/mnt/bind_ro";
        var fileName = "host_created.txt";
        var content = "bind-ro-host-content";

        await _sandbox!.Commands.RunAsync(
            $"mkdir -p {srcDir} {dest} && echo -n {content} > {srcDir}/{fileName}");

        var session = await _sandbox.Isolation.CreateAsync(
            new CreateIsolatedSessionRequest(
                new IsolatedWorkspaceSpec("/tmp", "rw"),
                Binds: new List<BindMount> { new BindMount(srcDir, dest, true) }));
        try
        {
            var exec = await session.RunAsync($"cat {dest}/{fileName}");
            Assert.Contains(content, StdoutText(exec));

            var write = await session.RunAsync(
                $"echo x > {dest}/newfile.txt 2>&1; echo EXIT=$?");
            var text = StdoutText(write);
            Assert.True(
                text.Contains("EXIT=1") || text.Contains("Read-only")
                    || text.Contains("read-only") || text.Contains("Permission denied"),
                $"expected write to fail through read-only bind, got: {text}");
        }
        finally
        {
            await session.DeleteAsync();
            await _sandbox.Commands.RunAsync($"rm -rf {srcDir}");
        }
    }
}
