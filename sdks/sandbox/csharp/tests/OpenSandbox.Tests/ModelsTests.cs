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

using System.Text.Json;
using FluentAssertions;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class ModelsTests
{
    [Fact]
    public void Execution_ShouldInitializeWithEmptyCollections()
    {
        // Arrange & Act
        var execution = new Execution();

        // Assert
        execution.Id.Should().BeNull();
        execution.ExecutionCount.Should().BeNull();
        execution.Logs.Should().NotBeNull();
        execution.Logs.Stdout.Should().BeEmpty();
        execution.Logs.Stderr.Should().BeEmpty();
        execution.Results.Should().BeEmpty();
        execution.Error.Should().BeNull();
        execution.Complete.Should().BeNull();
        execution.ExitCode.Should().BeNull();
    }

    [Fact]
    public void ExecutionLogs_ShouldAllowAddingMessages()
    {
        // Arrange
        var logs = new ExecutionLogs();
        var stdoutMsg = new OutputMessage { Text = "stdout", Timestamp = 1000, IsError = false };
        var stderrMsg = new OutputMessage { Text = "stderr", Timestamp = 2000, IsError = true };

        // Act
        logs.Stdout.Add(stdoutMsg);
        logs.Stderr.Add(stderrMsg);

        // Assert
        logs.Stdout.Should().HaveCount(1);
        logs.Stdout[0].Text.Should().Be("stdout");
        logs.Stderr.Should().HaveCount(1);
        logs.Stderr[0].Text.Should().Be("stderr");
        logs.Stderr[0].IsError.Should().BeTrue();
    }

    [Fact]
    public void OutputMessage_ShouldStoreProperties()
    {
        // Arrange & Act
        var msg = new OutputMessage
        {
            Text = "Hello World",
            Timestamp = 1234567890,
            IsError = false
        };

        // Assert
        msg.Text.Should().Be("Hello World");
        msg.Timestamp.Should().Be(1234567890);
        msg.IsError.Should().BeFalse();
    }

    [Fact]
    public void ExecutionResult_ShouldStoreProperties()
    {
        // Arrange & Act
        var result = new ExecutionResult
        {
            Text = "Result text",
            Timestamp = 1234567890,
            Raw = new Dictionary<string, object> { ["text/plain"] = "Result text" }
        };

        // Assert
        result.Text.Should().Be("Result text");
        result.Timestamp.Should().Be(1234567890);
        result.Raw.Should().ContainKey("text/plain");
    }

    [Fact]
    public void ExecutionError_ShouldStoreProperties()
    {
        // Arrange & Act
        var error = new ExecutionError
        {
            Name = "ValueError",
            Value = "Invalid value",
            Timestamp = 1234567890,
            Traceback = new List<string> { "line 1", "line 2" }
        };

        // Assert
        error.Name.Should().Be("ValueError");
        error.Value.Should().Be("Invalid value");
        error.Timestamp.Should().Be(1234567890);
        error.Traceback.Should().HaveCount(2);
    }

    [Fact]
    public void ExecutionComplete_ShouldStoreProperties()
    {
        // Arrange & Act
        var complete = new ExecutionComplete
        {
            Timestamp = 1234567890,
            ExecutionTimeMs = 500
        };

        // Assert
        complete.Timestamp.Should().Be(1234567890);
        complete.ExecutionTimeMs.Should().Be(500);
    }

    [Fact]
    public void SandboxInfo_ShouldStoreProperties()
    {
        // Arrange & Act
        var info = new SandboxInfo
        {
            Id = "sandbox-123",
            Image = new ImageSpec { Uri = "ubuntu:latest" },
            Platform = new PlatformSpec { Os = "linux", Arch = "amd64" },
            Entrypoint = new List<string> { "tail", "-f", "/dev/null" },
            Status = new SandboxStatus { State = "Running" },
            CreatedAt = DateTime.UtcNow,
            ExpiresAt = DateTime.UtcNow.AddMinutes(10),
            Metadata = new Dictionary<string, string> { ["key"] = "value" }
        };

        // Assert
        info.Id.Should().Be("sandbox-123");
        info.Image.Uri.Should().Be("ubuntu:latest");
        info.Platform.Should().NotBeNull();
        info.Platform!.Os.Should().Be("linux");
        info.Entrypoint.Should().HaveCount(3);
        info.Status.State.Should().Be("Running");
        info.Metadata.Should().ContainKey("key");
    }

    [Fact]
    public void SandboxStatus_ShouldStoreProperties()
    {
        // Arrange & Act
        var status = new SandboxStatus
        {
            State = "Error",
            Reason = "ImagePullFailed",
            Message = "Failed to pull image"
        };

        // Assert
        status.State.Should().Be("Error");
        status.Reason.Should().Be("ImagePullFailed");
        status.Message.Should().Be("Failed to pull image");
    }

    [Fact]
    public void ImageSpec_WithAuth_ShouldStoreCredentials()
    {
        // Arrange & Act
        var image = new ImageSpec
        {
            Uri = "private-registry.com/image:tag",
            Auth = new ImageAuth
            {
                Username = "user",
                Password = "pass"
            }
        };

        // Assert
        image.Uri.Should().Be("private-registry.com/image:tag");
        image.Auth.Should().NotBeNull();
        image.Auth!.Username.Should().Be("user");
        image.Auth.Password.Should().Be("pass");
    }

    [Fact]
    public void NetworkPolicy_ShouldStoreRules()
    {
        // Arrange & Act
        var policy = new NetworkPolicy
        {
            DefaultAction = NetworkRuleAction.Deny,
            Egress = new List<NetworkRule>
            {
                new() { Action = NetworkRuleAction.Allow, Target = "example.com" },
                new() { Action = NetworkRuleAction.Allow, Target = "*.trusted.com" }
            }
        };

        // Assert
        policy.DefaultAction.Should().Be(NetworkRuleAction.Deny);
        policy.Egress.Should().HaveCount(2);
        policy.Egress![0].Action.Should().Be(NetworkRuleAction.Allow);
        policy.Egress[0].Target.Should().Be("example.com");
    }

    [Fact]
    public void Volume_WithOssfs_ShouldSerializeExpectedPayload()
    {
        var request = new CreateSandboxRequest
        {
            Image = new ImageSpec { Uri = "python:3.11" },
            ResourceLimits = new Dictionary<string, string>(),
            Entrypoint = new List<string> { "python" },
            Platform = new PlatformSpec { Os = "linux", Arch = "arm64" },
            Volumes = new List<Volume>
            {
                new()
                {
                    Name = "oss-data",
                    MountPath = "/mnt/oss",
                    SubPath = "prefix",
                    Ossfs = new OSSFS
                    {
                        Bucket = "bucket-a",
                        Endpoint = "oss-cn-hangzhou.aliyuncs.com",
                        AccessKeyId = "ak",
                        AccessKeySecret = "sk",
                        Options = new List<string> { "allow_other" }
                    }
                }
            }
        };

        string json = JsonSerializer.Serialize(request);

        json.Should().Contain("\"ossfs\":");
        json.Should().Contain("\"bucket\":\"bucket-a\"");
        json.Should().Contain("\"endpoint\":\"oss-cn-hangzhou.aliyuncs.com\"");
        json.Should().Contain("\"accessKeyId\":\"ak\"");
        json.Should().Contain("\"accessKeySecret\":\"sk\"");
        json.Should().Contain("\"version\":\"2.0\"");
        json.Should().Contain("\"platform\":{\"os\":\"linux\",\"arch\":\"arm64\"}");
    }

    [Fact]
    public void SandboxMetrics_ShouldStoreProperties()
    {
        // Arrange & Act
        var metrics = new SandboxMetrics
        {
            CpuCount = 4,
            CpuUsedPercentage = 25.5,
            MemoryTotalMiB = 8192,
            MemoryUsedMiB = 2048,
            Timestamp = 1234567890
        };

        // Assert
        metrics.CpuCount.Should().Be(4);
        metrics.CpuUsedPercentage.Should().Be(25.5);
        metrics.MemoryTotalMiB.Should().Be(8192);
        metrics.MemoryUsedMiB.Should().Be(2048);
        metrics.Timestamp.Should().Be(1234567890);
    }

    [Fact]
    public void SandboxFileInfo_ShouldStoreProperties()
    {
        // Arrange & Act
        var fileInfo = new SandboxFileInfo
        {
            Path = "/tmp/test.txt",
            Size = 1024,
            Mode = 644,
            Owner = "root",
            Group = "root",
            CreatedAt = DateTime.UtcNow,
            ModifiedAt = DateTime.UtcNow
        };

        // Assert
        fileInfo.Path.Should().Be("/tmp/test.txt");
        fileInfo.Size.Should().Be(1024);
        fileInfo.Mode.Should().Be(644);
        fileInfo.Owner.Should().Be("root");
    }

    [Fact]
    public void WriteEntry_ShouldStoreProperties()
    {
        // Arrange & Act
        var entry = new WriteEntry
        {
            Path = "/tmp/file.txt",
            Data = "Hello World",
            Mode = 644,
            Owner = "user",
            Group = "group"
        };

        // Assert
        entry.Path.Should().Be("/tmp/file.txt");
        entry.Data.Should().Be("Hello World");
        entry.Mode.Should().Be(644);
    }

    [Fact]
    public void SearchEntry_ShouldStoreProperties()
    {
        // Arrange & Act
        var entry = new SearchEntry
        {
            Path = "/tmp",
            Pattern = "*.txt"
        };

        // Assert
        entry.Path.Should().Be("/tmp");
        entry.Pattern.Should().Be("*.txt");
    }

    [Fact]
    public void MoveEntry_ShouldStoreProperties()
    {
        // Arrange & Act
        var entry = new MoveEntry
        {
            Src = "/tmp/old.txt",
            Dest = "/tmp/new.txt"
        };

        // Assert
        entry.Src.Should().Be("/tmp/old.txt");
        entry.Dest.Should().Be("/tmp/new.txt");
    }

    [Fact]
    public void RunCommandOptions_ShouldStoreProperties()
    {
        // Arrange & Act
        var options = new RunCommandOptions
        {
            WorkingDirectory = "/home/user",
            Background = true,
            TimeoutSeconds = 30,
            Uid = 1000,
            Gid = 1000,
            Envs = new Dictionary<string, string>
            {
                ["APP_ENV"] = "test"
            }
        };

        // Assert
        options.WorkingDirectory.Should().Be("/home/user");
        options.Background.Should().BeTrue();
        options.TimeoutSeconds.Should().Be(30);
        options.Uid.Should().Be(1000);
        options.Gid.Should().Be(1000);
        options.Envs.Should().ContainKey("APP_ENV");
    }

    [Fact]
    public void CreateSessionOptions_ShouldStoreWorkingDirectory()
    {
        var options = new CreateSessionOptions
        {
            WorkingDirectory = "/workspace"
        };

        options.WorkingDirectory.Should().Be("/workspace");
    }

    [Fact]
    public void RunInSessionOptions_ShouldStoreProperties()
    {
        var options = new RunInSessionOptions
        {
            WorkingDirectory = "/workspace",
            TimeoutSeconds = 5
        };

        options.WorkingDirectory.Should().Be("/workspace");
        options.TimeoutSeconds.Should().Be(5);
    }

    [Fact]
    public void ServerStreamEvent_ShouldStoreProperties()
    {
        // Arrange & Act
        var ev = new ServerStreamEvent
        {
            Type = "stdout",
            Text = "output text",
            Timestamp = 1234567890,
            ExecutionCount = 1,
            ExecutionTime = 100
        };

        // Assert
        ev.Type.Should().Be("stdout");
        ev.Text.Should().Be("output text");
        ev.Timestamp.Should().Be(1234567890);
        ev.ExecutionCount.Should().Be(1);
        ev.ExecutionTime.Should().Be(100);
    }

    [Fact]
    public void CommandStatus_ShouldStoreProperties()
    {
        var startedAt = DateTime.UtcNow.AddSeconds(-5);
        var finishedAt = DateTime.UtcNow;
        var status = new CommandStatus
        {
            Id = "cmd-1",
            Content = "echo hello",
            Running = false,
            ExitCode = 0,
            Error = null,
            StartedAt = startedAt,
            FinishedAt = finishedAt
        };

        status.Id.Should().Be("cmd-1");
        status.Content.Should().Be("echo hello");
        status.Running.Should().BeFalse();
        status.ExitCode.Should().Be(0);
        status.StartedAt.Should().Be(startedAt);
        status.FinishedAt.Should().Be(finishedAt);
    }

    [Fact]
    public void CommandLogs_ShouldStoreProperties()
    {
        var logs = new CommandLogs
        {
            Content = "line1\nline2\n",
            Cursor = 12
        };

        logs.Content.Should().Contain("line1");
        logs.Cursor.Should().Be(12);
    }

    [Fact]
    public void PaginationInfo_ShouldStoreProperties()
    {
        // Arrange & Act
        var pagination = new PaginationInfo
        {
            Page = 1,
            PageSize = 10,
            TotalItems = 100,
            TotalPages = 10,
            HasNextPage = true
        };

        // Assert
        pagination.Page.Should().Be(1);
        pagination.PageSize.Should().Be(10);
        pagination.TotalItems.Should().Be(100);
        pagination.TotalPages.Should().Be(10);
        pagination.HasNextPage.Should().BeTrue();
    }
}
