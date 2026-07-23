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
using OpenSandbox.Core;
using Xunit;

namespace OpenSandbox.Tests;

public class ConstantsTests
{
    [Fact]
    public void DefaultExecdPort_ShouldBe44772()
    {
        Constants.DefaultExecdPort.Should().Be(44772);
    }

    [Fact]
    public void DefaultEntrypoint_ShouldBeTailCommand()
    {
        Constants.DefaultEntrypoint.Should().BeEquivalentTo(new[] { "tail", "-f", "/dev/null" });
    }

    [Fact]
    public void DefaultResourceLimits_ShouldContainCpuAndMemory()
    {
        Constants.DefaultResourceLimits.Should().ContainKey("cpu");
        Constants.DefaultResourceLimits.Should().ContainKey("memory");
        Constants.DefaultResourceLimits["cpu"].Should().Be("1");
        Constants.DefaultResourceLimits["memory"].Should().Be("2Gi");
    }

    [Fact]
    public void DefaultTimeoutSeconds_ShouldBe600()
    {
        Constants.DefaultTimeoutSeconds.Should().Be(600);
    }

    [Fact]
    public void DefaultReadyTimeoutSeconds_ShouldBe30()
    {
        Constants.DefaultReadyTimeoutSeconds.Should().Be(30);
    }

    [Fact]
    public void DefaultHealthCheckPollingIntervalMillis_ShouldBe200()
    {
        Constants.DefaultHealthCheckPollingIntervalMillis.Should().Be(200);
    }

    [Fact]
    public void DefaultRequestTimeoutSeconds_ShouldBe30()
    {
        Constants.DefaultRequestTimeoutSeconds.Should().Be(30);
    }

    [Fact]
    public void EnvDomain_ShouldBeCorrect()
    {
        Constants.EnvDomain.Should().Be("OPEN_SANDBOX_DOMAIN");
    }

    [Fact]
    public void EnvApiKey_ShouldBeCorrect()
    {
        Constants.EnvApiKey.Should().Be("OPEN_SANDBOX_API_KEY");
    }

    [Fact]
    public void ApiKeyHeader_ShouldBeCorrect()
    {
        Constants.ApiKeyHeader.Should().Be("OPEN-SANDBOX-API-KEY");
    }

    [Fact]
    public void RequestIdHeader_ShouldBeCorrect()
    {
        Constants.RequestIdHeader.Should().Be("x-request-id");
    }

    // Regression: the User-Agent version is hand-maintained and must be bumped
    // together with the package version. Update this expectation when releasing.
    [Fact]
    public void DefaultUserAgent_ShouldMatchPackageVersion()
    {
        Constants.DefaultUserAgent.Should().Be("OpenSandbox-CSharp-SDK/0.1.4");
    }
}
