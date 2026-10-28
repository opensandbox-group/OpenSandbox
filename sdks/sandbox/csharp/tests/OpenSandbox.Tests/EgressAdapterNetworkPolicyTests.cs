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

using System.Net;
using System.Text;
using System.Text.Json;
using OpenSandbox.Adapters;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

/// <summary>
/// Port-scoped egress rule coverage: sidecar read shape ("target": ""),
/// PATCH omits unset fields, and the read-lenient / write-strict port-range
/// asymmetry is pinned on purpose.
/// </summary>
public class EgressAdapterNetworkPolicyTests
{
    private const string OkResponse = """{ "status": "ok" }""";

    private const string OkPolicyWithPortOnlyRule = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "deny", "target": "", "ports": [25] }
            ]
          }
        }
        """;

    private const string OkPolicyWithTargetAndPorts = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "10.0.0.5", "ports": [22, 80] }
            ]
          }
        }
        """;

    private const string OkPolicyWithBoundaryPorts = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "ports": [1, 65535] }
            ]
          }
        }
        """;

    private const string OkPolicyWithNullPorts = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "example.com", "ports": null }
            ]
          }
        }
        """;

    private const string OkPolicyWithMalformedPort = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "ports": ["https"] }
            ]
          }
        }
        """;

    private const string OkPolicyWithEmptyPorts = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "deny", "ports": [] }
            ]
          }
        }
        """;

    private const string OkPolicyWithNonArrayPorts = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "ports": 443 }
            ]
          }
        }
        """;

    private const string OkPolicyWithOutOfRangePort = """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "example.com", "ports": [70000] }
            ]
          }
        }
        """;

    [Fact]
    public async Task GetPolicyAsync_ParsesPortOnlyRule_WithSidecarWireShape()
    {
        var (adapter, _) = CreateAdapter(OkPolicyWithPortOnlyRule);

        var policy = await adapter.GetPolicyAsync();

        var rule = Assert.Single(policy.Egress!);
        Assert.Null(rule.Target);
        Assert.Equal(new List<int> { 25 }, rule.Ports);
    }

    [Fact]
    public async Task GetPolicyAsync_ParsesTargetWithPortsRule()
    {
        var (adapter, _) = CreateAdapter(OkPolicyWithTargetAndPorts);

        var policy = await adapter.GetPolicyAsync();

        var rule = Assert.Single(policy.Egress!);
        Assert.Equal("10.0.0.5", rule.Target);
        Assert.Equal(new List<int> { 22, 80 }, rule.Ports);
    }

    [Fact]
    public async Task GetPolicyAsync_ParsesBoundaryPorts()
    {
        var (adapter, _) = CreateAdapter(OkPolicyWithBoundaryPorts);

        var policy = await adapter.GetPolicyAsync();

        Assert.Equal(new List<int> { 1, 65535 }, policy.Egress![0].Ports);
    }

    [Fact]
    public async Task GetPolicyAsync_NullPorts_ParsesAsNotPortScoped_AndRoundTrips()
    {
        var (adapter, handler) = CreateAdapter(OkPolicyWithNullPorts);

        var policy = await adapter.GetPolicyAsync();

        var rule = Assert.Single(policy.Egress!);
        Assert.Null(rule.Ports);
        Assert.Equal("example.com", rule.Target);

        await adapter.PatchRulesAsync(policy.Egress!);
        var patch = Assert.Single(handler.Requests, r => r.Method == HttpMethod.Patch);
        using var body = JsonDocument.Parse(patch.Body!);
        var parsed = Assert.Single(body.RootElement.EnumerateArray());
        Assert.Equal("example.com", parsed.GetProperty("target").GetString());
        Assert.False(parsed.TryGetProperty("ports", out _));
    }

    [Fact]
    public async Task GetPolicyAsync_PortOnlyRule_RoundTripsThroughPatchRules()
    {
        // The real GET→PATCH round trip: the parsed port-only rule must
        // re-PATCH without a target key (never as null or "").
        var (adapter, handler) = CreateAdapter(OkPolicyWithPortOnlyRule);

        var policy = await adapter.GetPolicyAsync();
        await adapter.PatchRulesAsync(policy.Egress!);

        var patch = Assert.Single(handler.Requests, r => r.Method == HttpMethod.Patch);
        using var body = JsonDocument.Parse(patch.Body!);
        var rule = Assert.Single(body.RootElement.EnumerateArray());
        Assert.Equal("deny", rule.GetProperty("action").GetString());
        Assert.Equal(25, rule.GetProperty("ports")[0].GetInt32());
        Assert.False(rule.TryGetProperty("target", out _));
    }

    [Fact]
    public async Task GetPolicyAsync_EmptyPortsList_ThrowsContractError()
    {
        // The spec declares minItems: 1; accepting "ports": [] on read would
        // let a GET→PATCH round-trip silently rewrite the rule to "no ports".
        var (adapter, _) = CreateAdapter(OkPolicyWithEmptyPorts);

        var ex = await Assert.ThrowsAsync<SandboxApiException>(() => adapter.GetPolicyAsync());
        Assert.Contains("ports must not be empty", ex.Message);
    }

    [Fact]
    public async Task GetPolicyAsync_NonArrayPorts_ThrowsContractError()
    {
        var (adapter, _) = CreateAdapter(OkPolicyWithNonArrayPorts);

        var ex = await Assert.ThrowsAsync<SandboxApiException>(() => adapter.GetPolicyAsync());
        Assert.Contains("Invalid ports in network rule", ex.Message);
    }

    [Fact]
    public async Task GetPolicyAsync_ReadsAreLenientOnPortRanges_WritesAreNot()
    {
        // Read side accepts an out-of-range port (range validation is a
        // write-side concern); re-PATCHing the parsed rule throws — the
        // asymmetry is pinned here on purpose.
        var (adapter, _) = CreateAdapter(OkPolicyWithOutOfRangePort);

        var policy = await adapter.GetPolicyAsync();
        Assert.Equal(new List<int> { 70000 }, policy.Egress![0].Ports);

        var ex = Assert.Throws<ArgumentException>(
            () => NetworkPolicyCodec.ToRulesPayload(policy.Egress!));
        Assert.Contains("NetworkRule[0] port 70000 out of range", ex.Message);
    }

    [Fact]
    public async Task GetPolicyAsync_ServerError_ThrowsSandboxApiException()
    {
        var (adapter, _) = CreateAdapter("{}", "500");

        var ex = await Assert.ThrowsAsync<SandboxApiException>(() => adapter.GetPolicyAsync());
        Assert.Equal(500, ex.StatusCode);
    }

    [Fact]
    public async Task PatchRulesAsync_OmitsTargetKey_ForPortOnlyRule()
    {
        var (adapter, handler) = CreateAdapter(OkResponse);

        await adapter.PatchRulesAsync(new[] { new NetworkRule { Action = NetworkRuleAction.Deny, Ports = new List<int> { 25 } } });

        var request = Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Patch, request.Method);
        Assert.Equal("/policy", request.Path);
        using var body = JsonDocument.Parse(request.Body!);
        var rule = Assert.Single(body.RootElement.EnumerateArray());
        Assert.Equal("deny", rule.GetProperty("action").GetString());
        Assert.Equal(25, rule.GetProperty("ports")[0].GetInt32());
        Assert.False(rule.TryGetProperty("target", out _));
    }

    [Fact]
    public async Task PatchRulesAsync_BlankTargetWithPorts_IsPortOnly()
    {
        var (adapter, handler) = CreateAdapter(OkResponse);

        await adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "  ", Ports = new List<int> { 22 } },
        });

        var request = Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Patch, request.Method);
        Assert.Equal("/policy", request.Path);
        using var body = JsonDocument.Parse(request.Body!);
        var rule = Assert.Single(body.RootElement.EnumerateArray());
        Assert.False(rule.TryGetProperty("target", out _));
        Assert.Equal(22, rule.GetProperty("ports")[0].GetInt32());
    }

    [Fact]
    public async Task PatchRulesAsync_PlainTargetRule_Unchanged()
    {
        var (adapter, handler) = CreateAdapter(OkResponse);

        await adapter.PatchRulesAsync(new[] { new NetworkRule { Action = NetworkRuleAction.Allow, Target = "example.com" } });

        var request = Assert.Single(handler.Requests);
        Assert.Equal(HttpMethod.Patch, request.Method);
        Assert.Equal("/policy", request.Path);
        using var body = JsonDocument.Parse(request.Body!);
        var rule = Assert.Single(body.RootElement.EnumerateArray());
        Assert.Equal("example.com", rule.GetProperty("target").GetString());
        Assert.False(rule.TryGetProperty("ports", out _));
    }

    [Fact]
    public async Task PatchRulesAsync_TargetWithPorts_SendsBoth()
    {
        var (adapter, handler) = CreateAdapter(OkResponse);

        await adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "10.0.0.5", Ports = new List<int> { 22, 80 } },
        });

        var request = Assert.Single(handler.Requests);
        using var body = JsonDocument.Parse(request.Body!);
        var rule = Assert.Single(body.RootElement.EnumerateArray());
        Assert.Equal("10.0.0.5", rule.GetProperty("target").GetString());
        Assert.Equal(22, rule.GetProperty("ports")[0].GetInt32());
        Assert.Equal(80, rule.GetProperty("ports")[1].GetInt32());
    }

    [Fact]
    public async Task PatchRulesAsync_NeitherTargetNorPorts_FailsFast()
    {
        var (adapter, _) = CreateAdapter(OkResponse);

        var ex = await Assert.ThrowsAsync<ArgumentException>(
            () => adapter.PatchRulesAsync(new[] { new NetworkRule { Action = NetworkRuleAction.Allow } }));
        Assert.Contains("NetworkRule[0] requires target, ports, or both", ex.Message);
    }

    [Fact]
    public async Task PatchRulesAsync_EmptyPortsList_FailsLikeTheServer()
    {
        var (adapter, _) = CreateAdapter(OkResponse);

        var ex = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "example.com", Ports = new List<int>() },
        }));
        Assert.Contains("NetworkRule[0] has empty ports", ex.Message);
    }

    [Fact]
    public async Task PatchRulesAsync_InvalidPortValues_FailFastWithIndex()
    {
        var (adapter, _) = CreateAdapter(OkResponse);

        var ex = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "example.com" },
            new NetworkRule { Action = NetworkRuleAction.Deny, Ports = new List<int> { 0, 443 } },
        }));
        Assert.Contains("NetworkRule[1] port 0 out of range", ex.Message);

        var ex2 = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Deny, Ports = new List<int> { 443, 443 } },
        }));
        Assert.Contains("NetworkRule[0] has duplicate port 443", ex2.Message);
    }

    [Fact]
    public async Task PatchRulesAsync_OutOfRangeBoundary_AndCapOverflow_FailFast()
    {
        var (adapter, _) = CreateAdapter(OkResponse);

        var ex = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Deny, Ports = new List<int> { 65536 } },
        }));
        Assert.Contains("NetworkRule[0] port 65536 out of range", ex.Message);

        var ex2 = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Ports = Enumerable.Range(1, 257).ToList() },
        }));
        Assert.Contains("NetworkRule[0] has 257 ports", ex2.Message);
    }

    [Fact]
    public async Task PatchRulesAsync_DomainTargetWithPorts_FailsLocally()
    {
        var (adapter, _) = CreateAdapter(OkResponse);

        var ex = await Assert.ThrowsAsync<ArgumentException>(() => adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "example.com", Ports = new List<int> { 443 } },
        }));
        Assert.Contains("ports are not supported for domain target", ex.Message);
    }

    [Fact]
    public async Task PatchRulesAsync_CapAndIps_Accepted()
    {
        var (adapter, handler) = CreateAdapter(OkResponse);

        await adapter.PatchRulesAsync(new[]
        {
            new NetworkRule { Action = NetworkRuleAction.Allow, Ports = Enumerable.Range(1, 256).ToList() },
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "10.0.0.0/8", Ports = new List<int> { 22 } },
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "2001:db8::1", Ports = new List<int> { 443 } },
        });

        var request = Assert.Single(handler.Requests);
        using var body = JsonDocument.Parse(request.Body!);
        Assert.Equal(3, body.RootElement.EnumerateArray().Count());
    }

    private static (EgressAdapter Adapter, CaptureHandler Handler) CreateAdapter(string responseBody)
    {
        return CreateAdapter(responseBody, "ok");
    }

    private static (EgressAdapter Adapter, CaptureHandler Handler) CreateAdapter(string responseBody, string getStatus)
    {
        var handler = new CaptureHandler(responseBody, getStatus);
        var wrapper = new HttpClientWrapper(
            new HttpClient(handler),
            "http://egress.local",
            new Dictionary<string, string>());
        return (new EgressAdapter(wrapper), handler);
    }

    private sealed class CaptureHandler : HttpMessageHandler
    {
        private readonly string _getBody;
        private readonly HttpStatusCode _getStatus;

        public CaptureHandler(string responseBody)
            : this(responseBody, "ok")
        {
        }

        public CaptureHandler(string getBody, string getStatus)
        {
            _getBody = getBody;
            _getStatus = getStatus == "500" ? HttpStatusCode.InternalServerError : HttpStatusCode.OK;
        }

        public List<(HttpMethod Method, string? Path, string? Body)> Requests { get; } = new();

        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            string? body = null;
            if (request.Content != null)
            {
                body = await request.Content.ReadAsStringAsync().ConfigureAwait(false);
            }
            Requests.Add((request.Method, request.RequestUri?.PathAndQuery, body));

            var isGet = request.Method == HttpMethod.Get;
            return new HttpResponseMessage(isGet ? _getStatus : HttpStatusCode.OK)
            {
                Content = new StringContent(isGet ? _getBody : OkResponse, Encoding.UTF8, "application/json"),
            };
        }
    }
}
