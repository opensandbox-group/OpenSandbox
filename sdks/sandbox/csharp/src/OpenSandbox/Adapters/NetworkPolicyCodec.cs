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

using System.Text.Json;
using System.Linq;
using OpenSandbox.Core;
using OpenSandbox.Models;

namespace OpenSandbox.Adapters;

/// <summary>
/// Shared JSON mapping between <see cref="NetworkPolicy"/>/<see cref="NetworkRule"/>
/// and the egress policy wire format used by both the sandbox-side egress sidecar
/// and the lifecycle networkpolicy control-plane API.
/// </summary>
internal static class NetworkPolicyCodec
{
    public static NetworkPolicy ParsePolicy(JsonElement element)
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

    public static List<Dictionary<string, object?>> ToRulesPayload(IReadOnlyList<NetworkRule> rules)
    {
        return rules.Select(r => new Dictionary<string, object?>
        {
            ["action"] = r.Action == NetworkRuleAction.Allow ? "allow" : "deny",
            ["target"] = r.Target
        }).ToList();
    }

    public static NetworkRule ParseNetworkRule(JsonElement element)
    {
        var actionText = element.GetProperty("action").GetString();
        var target = element.GetProperty("target").GetString();
        return new NetworkRule
        {
            Action = ParseNetworkRuleAction(actionText),
            Target = target ?? throw new SandboxApiException("Missing target in network rule")
        };
    }

    public static NetworkRuleAction ParseNetworkRuleAction(string? action)
    {
        return action?.ToLowerInvariant() switch
        {
            "allow" => NetworkRuleAction.Allow,
            "deny" => NetworkRuleAction.Deny,
            _ => throw new SandboxApiException($"Invalid network rule action: {action ?? "<null>"}")
        };
    }
}
