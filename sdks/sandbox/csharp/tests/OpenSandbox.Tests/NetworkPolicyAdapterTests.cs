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
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

public class NetworkPolicyAdapterTests
{
    [Fact]
    public async Task GetPolicyAsync_ShouldParseControlPlanePolicy()
    {
        const string payload = """
        {
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "pypi.org" }
            ]
          },
          "enforcementMode": "enforce"
        }
        """;
        var handler = new CapturingHandler(payload);
        var adapter = CreateAdapter(handler, "fsb-1");

        var policy = await adapter.GetPolicyAsync();

        handler.Method.Should().Be(HttpMethod.Get);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        policy.DefaultAction.Should().Be(NetworkRuleAction.Deny);
        policy.Egress.Should().ContainSingle();
        policy.Egress![0].Action.Should().Be(NetworkRuleAction.Allow);
        policy.Egress[0].Target.Should().Be("pypi.org");
    }

    [Fact]
    public async Task PatchRulesAsync_ShouldSendRulesBody()
    {
        var handler = new CapturingHandler("{}");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.PatchRulesAsync(
        [
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "www.github.com" }
        ]);

        handler.Method.Should().Be(HttpMethod.Patch);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        var rule = json.RootElement[0];
        rule.GetProperty("action").GetString().Should().Be("allow");
        rule.GetProperty("target").GetString().Should().Be("www.github.com");
    }

    [Fact]
    public async Task DeleteRulesAsync_ShouldSendTargetsBody()
    {
        var handler = new CapturingHandler("{}");
        var adapter = CreateAdapter(handler, "fsb-1");

        await adapter.DeleteRulesAsync(["www.github.com", "*.blocked.org"]);

        handler.Method.Should().Be(HttpMethod.Delete);
        handler.PathAndQuery.Should().Be("/v1/sandboxes/fsb-1/networkpolicy");
        handler.RequestBody.Should().NotBeNullOrEmpty();
        using var json = JsonDocument.Parse(handler.RequestBody!);
        json.RootElement.GetArrayLength().Should().Be(2);
        json.RootElement[0].GetString().Should().Be("www.github.com");
        json.RootElement[1].GetString().Should().Be("*.blocked.org");
    }

    private static NetworkPolicyAdapter CreateAdapter(CapturingHandler handler, string sandboxId)
    {
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://localhost:8080/v1");
        return new NetworkPolicyAdapter(wrapper, sandboxId);
    }

    private sealed class CapturingHandler(string payload) : HttpMessageHandler
    {
        public HttpMethod? Method { get; private set; }

        public string? PathAndQuery { get; private set; }

        public string? RequestBody { get; private set; }

        protected override async Task<HttpResponseMessage> SendAsync(
            HttpRequestMessage request,
            CancellationToken cancellationToken)
        {
            Method = request.Method;
            PathAndQuery = request.RequestUri?.PathAndQuery;
            RequestBody = request.Content is null
                ? null
                : await request.Content.ReadAsStringAsync();
            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payload, Encoding.UTF8, "application/json")
            };
            return response;
        }
    }
}
