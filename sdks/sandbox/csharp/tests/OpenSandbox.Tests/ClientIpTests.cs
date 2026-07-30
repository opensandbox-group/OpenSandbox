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

using System.Net;
using FluentAssertions;
using OpenSandbox.Config;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using Xunit;

namespace OpenSandbox.Tests;

public class ClientIpTests : IDisposable
{
    public ClientIpTests()
    {
        ClientIpDetector.ResetForTest();
    }

    public void Dispose()
    {
        ClientIpDetector.ResetForTest();
    }

    private static ConnectionConfig ConfigWith(
        string? userClientIp = null,
        string headerName = Constants.ClientIpHeader)
    {
        var headers = new Dictionary<string, string>();
        if (userClientIp != null)
        {
            headers[headerName] = userClientIp;
        }
        return new ConnectionConfig(new ConnectionConfigOptions { Headers = headers });
    }

    private static string? ClientIpDefaultHeader(HttpClient client)
    {
        return client.DefaultRequestHeaders.TryGetValues(Constants.ClientIpHeader, out var values)
            ? string.Join(",", values)
            : null;
    }

    [Fact]
    public void DetectOutboundIp_ShouldReturnValidOrEmpty()
    {
        var ip = ClientIpDetector.DetectOutboundIp();
        if (string.IsNullOrEmpty(ip))
        {
            return; // best-effort: allowed in a network-less environment
        }

        IPAddress.TryParse(ip, out var parsed).Should().BeTrue();
        IPAddress.IsLoopback(parsed!).Should().BeFalse();
        parsed!.Equals(IPAddress.Any).Should().BeFalse();
    }

    [Fact]
    public void SelectClientIp_ShouldPreferNamedNicAndSkipVirtual()
    {
        var nics = new List<ClientIpDetector.NicAddrs>
        {
            new("docker0", new[] { "172.17.0.1" }), // virtual, skip
            new("utun3", new[] { "10.8.0.3" }), // VPN, skip
            new("en0", new[] { "10.1.1.1" }), // main NIC
            new("eth1", new[] { "192.168.5.5" }),
        };
        ClientIpDetector.SelectClientIp(nics).Should().Be("10.1.1.1");
    }

    [Fact]
    public void SelectClientIp_ShouldFallBackToPrivateWhenNoNamedNic()
    {
        var nics = new List<ClientIpDetector.NicAddrs>
        {
            new("docker0", new[] { "172.17.0.1" }), // virtual, skip
            new("custom9", new[] { "8.8.4.4" }), // public, lower preference
            new("wlan9", new[] { "10.0.0.50" }), // private, preferred
        };
        ClientIpDetector.SelectClientIp(nics).Should().Be("10.0.0.50");
    }

    [Fact]
    public void SelectClientIp_ShouldSkipLoopbackAndLinkLocal()
    {
        var nics = new List<ClientIpDetector.NicAddrs>
        {
            new("eth0", new[] { "127.0.0.1", "169.254.1.1", "10.1.2.3" }),
        };
        ClientIpDetector.SelectClientIp(nics).Should().Be("10.1.2.3");
    }

    [Fact]
    public void SelectClientIp_ShouldReturnEmptyWhenNoUsable()
    {
        var nics = new List<ClientIpDetector.NicAddrs>
        {
            new("docker0", new[] { "172.17.0.1" }),
            new("eth0", new[] { "169.254.1.1" }),
        };
        ClientIpDetector.SelectClientIp(nics).Should().Be(string.Empty);
    }

    [Fact]
    public void ClientIp_ShouldCacheDetectedValue()
    {
        ClientIpDetector.Detector = () => "10.1.2.3";
        ClientIpDetector.ClientIp().Should().Be("10.1.2.3");

        // Cache must hold even after the detector changes.
        ClientIpDetector.Detector = () => "10.9.9.9";
        ClientIpDetector.ClientIp().Should().Be("10.1.2.3");
    }

    [Fact]
    public void CreateHttpClient_ShouldSetClientIpDefaultHeader()
    {
        ClientIpDetector.Detector = () => "10.9.8.7";
        using var client = ConfigWith().CreateHttpClient();
        ClientIpDefaultHeader(client).Should().Be("10.9.8.7");
    }

    [Fact]
    public void CreateSseHttpClient_ShouldSetClientIpDefaultHeader()
    {
        // The SSE client backs all streaming/raw sends that bypass HttpClientWrapper
        // (command SSE, isolated sessions, code run-stream), so it must carry the header too.
        ClientIpDetector.Detector = () => "10.9.8.7";
        using var client = ConfigWith().CreateSseHttpClient();
        ClientIpDefaultHeader(client).Should().Be("10.9.8.7");
    }

    [Fact]
    public void CreateHttpClient_ShouldNotOverrideUserProvidedClientIp()
    {
        // Seed via a lowercase header name to prove the guard is case-insensitive,
        // matching the JS/Python tests.
        ClientIpDetector.Detector = () => "10.9.8.7";
        using var client = ConfigWith(
            userClientIp: "192.168.0.9",
            headerName: "open-sandbox-client-ip").CreateHttpClient();

        client.DefaultRequestHeaders.GetValues(Constants.ClientIpHeader)
            .Should().ContainSingle().Which.Should().Be("192.168.0.9");
    }

    [Fact]
    public void CreateHttpClient_ShouldOmitClientIpWhenUndetectable()
    {
        ClientIpDetector.Detector = () => string.Empty;
        using var client = ConfigWith().CreateHttpClient();
        ClientIpDefaultHeader(client).Should().BeNull();
    }
}
