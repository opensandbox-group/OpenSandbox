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
using System.Text;
using FluentAssertions;
using OpenSandbox.Adapters;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using Xunit;

namespace OpenSandbox.Tests;

/// <summary>
/// Regression coverage for a port-scoped egress rule (no `target` in the wire
/// JSON): GetPropertyAsync used to call JsonElement.GetProperty("target"),
/// which throws when the property is absent instead of merely empty. A
/// port-only rule written by another language's SDK would crash this one on
/// read.
/// </summary>
public class EgressAdapterNetworkPolicyTests
{
    [Fact]
    public async Task GetPolicyAsync_ShouldParsePortOnlyRule_WithoutThrowing()
    {
        var handler = new CaptureHandler(_ => """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "deny", "ports": [25] }
            ]
          }
        }
        """);
        var adapter = CreateAdapter(handler);

        var policy = await adapter.GetPolicyAsync();

        policy.Egress.Should().ContainSingle();
        policy.Egress![0].Target.Should().BeNull();
        policy.Egress[0].Ports.Should().Equal(25);
    }

    [Fact]
    public async Task GetPolicyAsync_ShouldParseTargetWithPortsRule()
    {
        var handler = new CaptureHandler(_ => """
        {
          "status": "ok",
          "policy": {
            "defaultAction": "deny",
            "egress": [
              { "action": "allow", "target": "10.0.0.5", "ports": [22, 80] }
            ]
          }
        }
        """);
        var adapter = CreateAdapter(handler);

        var policy = await adapter.GetPolicyAsync();

        policy.Egress.Should().ContainSingle();
        policy.Egress![0].Target.Should().Be("10.0.0.5");
        policy.Egress[0].Ports.Should().Equal(22, 80);
    }

    [Fact]
    public async Task PatchRulesAsync_ShouldOmitTargetKey_ForPortOnlyRule()
    {
        var handler = new CaptureHandler(_ => """{ "status": "ok" }""");
        var adapter = CreateAdapter(handler);

        await adapter.PatchRulesAsync([
            new NetworkRule { Action = NetworkRuleAction.Deny, Ports = [25] }
        ]);

        handler.Requests.Should().ContainSingle();
        var body = handler.Requests[0].Body;
        body.Should().NotContain("\"target\"", "a port-only rule must not send target, not even as null");
        body.Should().Contain("\"ports\":[25]");
    }

    [Fact]
    public async Task PatchRulesAsync_ShouldStillSendPlainTargetRule_Unchanged()
    {
        var handler = new CaptureHandler(_ => """{ "status": "ok" }""");
        var adapter = CreateAdapter(handler);

        await adapter.PatchRulesAsync([
            new NetworkRule { Action = NetworkRuleAction.Allow, Target = "example.com" }
        ]);

        var body = handler.Requests[0].Body;
        body.Should().Contain("\"target\":\"example.com\"");
        body.Should().NotContain("\"ports\"");
    }

    private static EgressAdapter CreateAdapter(
        HttpMessageHandler handler,
        IReadOnlyDictionary<string, string>? headers = null)
    {
        var client = new HttpClient(handler);
        var wrapper = new HttpClientWrapper(client, "http://egress.local", headers);
        return new EgressAdapter(wrapper);
    }

    private sealed class CaptureHandler(Func<HttpRequestMessage, string> payloadSelector) : HttpMessageHandler
    {
        public List<CapturedRequest> Requests { get; } = [];

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            var body = request.Content == null
                ? null
                : await request.Content.ReadAsStringAsync().ConfigureAwait(false);
            Requests.Add(new CapturedRequest(request.Method, request.RequestUri?.PathAndQuery, body));

            var response = new HttpResponseMessage(HttpStatusCode.OK)
            {
                Content = new StringContent(payloadSelector(request), Encoding.UTF8, "application/json")
            };
            return response;
        }
    }

    private sealed record CapturedRequest(HttpMethod Method, string? PathAndQuery, string? Body);
}
