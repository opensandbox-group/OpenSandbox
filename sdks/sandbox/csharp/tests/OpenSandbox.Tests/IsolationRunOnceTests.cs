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
using OpenSandbox.Models;
using OpenSandbox.Services;
using Moq;
using Xunit;

namespace OpenSandbox.Tests;

public class IsolationRunOnceTests
{
    private static Mock<IIsolationSession> CreateMockSession(string sessionId = "sess-1")
    {
        var session = new Mock<IIsolationSession>();
        session.Setup(s => s.SessionId).Returns(sessionId);
        session.Setup(s => s.Info).Returns(new IsolatedSessionInfo(sessionId));
        session.Setup(s => s.RunAsync(It.IsAny<string>(), It.IsAny<IsolatedRunOpts?>(),
                It.IsAny<ExecutionHandlers?>(), It.IsAny<CancellationToken>()))
            .ReturnsAsync(new Execution());
        session.Setup(s => s.DeleteAsync(It.IsAny<CancellationToken>()))
            .Returns(Task.CompletedTask);
        return session;
    }

    private static Mock<IIsolatedSessions> CreateMockService(Mock<IIsolationSession> session)
    {
        var service = new Mock<IIsolatedSessions>();
        service.Setup(s => s.CreateAsync(It.IsAny<CreateIsolatedSessionRequest>(), It.IsAny<CancellationToken>()))
            .ReturnsAsync(session.Object);
        return service;
    }

    [Fact]
    public async Task RunOnceAsync_CreatesRunsAndDeletes()
    {
        var session = CreateMockSession();
        var service = CreateMockService(session);

        var result = await service.Object.RunOnceAsync("echo hello", "/workspace");

        result.Should().NotBeNull();
        service.Verify(s => s.CreateAsync(It.IsAny<CreateIsolatedSessionRequest>(), It.IsAny<CancellationToken>()), Times.Once);
        session.Verify(s => s.RunAsync("echo hello", null, null, It.IsAny<CancellationToken>()), Times.Once);
        session.Verify(s => s.DeleteAsync(It.IsAny<CancellationToken>()), Times.Once);
    }

    [Fact]
    public async Task RunOnceAsync_DeletesOnRunFailure()
    {
        var session = CreateMockSession();
        session.Setup(s => s.RunAsync(It.IsAny<string>(), It.IsAny<IsolatedRunOpts?>(),
                It.IsAny<ExecutionHandlers?>(), It.IsAny<CancellationToken>()))
            .ThrowsAsync(new InvalidOperationException("boom"));
        var service = CreateMockService(session);

        await Assert.ThrowsAsync<InvalidOperationException>(
            () => service.Object.RunOnceAsync("bad", "/workspace"));

        session.Verify(s => s.DeleteAsync(It.IsAny<CancellationToken>()), Times.Once);
    }

    [Fact]
    public async Task RunOnceAsync_ToleratesDeleteFailure()
    {
        var session = CreateMockSession();
        session.Setup(s => s.DeleteAsync(It.IsAny<CancellationToken>()))
            .ThrowsAsync(new InvalidOperationException("delete failed"));
        var service = CreateMockService(session);

        var result = await service.Object.RunOnceAsync("echo ok", "/workspace");

        result.Should().NotBeNull();
    }

    [Fact]
    public async Task WithSessionAsync_CallsFnAndDeletes()
    {
        var session = CreateMockSession();
        var service = CreateMockService(session);

        var result = await service.Object.WithSessionAsync(
            new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/workspace")),
            s =>
            {
                s.SessionId.Should().Be("sess-1");
                return Task.FromResult("done");
            });

        result.Should().Be("done");
        session.Verify(s => s.DeleteAsync(It.IsAny<CancellationToken>()), Times.Once);
    }

    [Fact]
    public async Task WithSessionAsync_DeletesOnException()
    {
        var session = CreateMockSession();
        var service = CreateMockService(session);

        await Assert.ThrowsAsync<InvalidOperationException>(
            () => service.Object.WithSessionAsync<string>(
                new CreateIsolatedSessionRequest(new IsolatedWorkspaceSpec("/workspace")),
                _ => throw new InvalidOperationException("callback error")));

        session.Verify(s => s.DeleteAsync(It.IsAny<CancellationToken>()), Times.Once);
    }
}
