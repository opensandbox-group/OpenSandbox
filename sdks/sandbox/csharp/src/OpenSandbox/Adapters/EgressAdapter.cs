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
using System.Linq;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

internal sealed class EgressAdapter : IEgress
{
    private readonly HttpClientWrapper _client;

    public EgressAdapter(HttpClientWrapper client)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
    }

    public async Task<NetworkPolicy> GetPolicyAsync(CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<JsonElement>("/policy", cancellationToken: cancellationToken).ConfigureAwait(false);
        if (!response.TryGetProperty("policy", out var policyElement) || policyElement.ValueKind != JsonValueKind.Object)
        {
            throw new SandboxApiException("Missing policy in egress response");
        }

        return ParseNetworkPolicy(policyElement);
    }

    public async Task PatchRulesAsync(
        IReadOnlyList<NetworkRule> rules,
        CancellationToken cancellationToken = default)
    {
        var normalizedRules = rules.Select(r => new Dictionary<string, object?>
        {
            ["action"] = r.Action == NetworkRuleAction.Allow ? "allow" : "deny",
            ["target"] = r.Target
        }).ToList();

        await _client.PatchAsync("/policy", normalizedRules, cancellationToken).ConfigureAwait(false);
    }

    private static NetworkPolicy ParseNetworkPolicy(JsonElement element)
    {
        var policy = new NetworkPolicy();

        if (element.TryGetProperty("defaultAction", out var defaultAction) &&
            defaultAction.ValueKind == JsonValueKind.String)
        {
            policy.DefaultAction = ParseNetworkRuleAction(defaultAction.GetString());
        }

        if (element.TryGetProperty("egress", out var egress) &&
            egress.ValueKind == JsonValueKind.Array)
        {
            policy.Egress = egress.EnumerateArray().Select(ParseNetworkRule).ToList();
        }

        return policy;
    }

    private static NetworkRule ParseNetworkRule(JsonElement element)
    {
        var actionText = element.GetProperty("action").GetString();
        var target = element.GetProperty("target").GetString();
        return new NetworkRule
        {
            Action = ParseNetworkRuleAction(actionText),
            Target = target ?? throw new SandboxApiException("Missing target in network rule")
        };
    }

    private static NetworkRuleAction ParseNetworkRuleAction(string? action)
    {
        return action?.ToLowerInvariant() switch
        {
            "allow" => NetworkRuleAction.Allow,
            "deny" => NetworkRuleAction.Deny,
            _ => throw new SandboxApiException($"Invalid network rule action: {action ?? "<null>"}")
        };
    }
}
