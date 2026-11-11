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
        var payload = new List<Dictionary<string, object?>>(rules.Count);
        for (var i = 0; i < rules.Count; i++)
        {
            payload.Add(ToRulesPayloadEntry(rules[i], i, nameof(rules)));
        }
        return payload;
    }

    /// <summary>
    /// Validates a rule the way the server would, so the most common mistakes
    /// — no target/ports, an empty or out-of-range ports list, duplicates,
    /// more than 256 ports, a domain target combined with ports — surface at
    /// the call site with the offending rule index instead of as a remote
    /// 422. Every wire path that serializes rules validates through this.
    /// </summary>
    public static void ValidateRules(IReadOnlyList<NetworkRule> rules, string? paramName = null)
    {
        for (var i = 0; i < rules.Count; i++)
        {
            ValidateRule(rules[i], i, paramName);
        }
    }

    private static void ValidateRule(NetworkRule rule, int index, string? paramName)
    {
        // Empty ports is contract-invalid (minItems: 1); silently treating
        // it as "no ports" would widen the rule.
        if (rule.Ports != null && rule.Ports.Count == 0)
        {
            throw new ArgumentException(
                $"NetworkRule[{index}] has empty ports; omit the field for a non-port-scoped rule.",
                paramName);
        }

        var hasTarget = !string.IsNullOrWhiteSpace(rule.Target);
        var hasPorts = rule.Ports is { Count: > 0 };
        if (!hasTarget && !hasPorts)
        {
            throw new ArgumentException(
                $"NetworkRule[{index}] requires target, ports, or both.", paramName);
        }
        if (hasPorts)
        {
            // Count before values: the server checks the cap first too.
            if (rule.Ports!.Count > MaxPortsPerRule)
            {
                throw new ArgumentException(
                    $"NetworkRule[{index}] has {rule.Ports.Count} ports; at most {MaxPortsPerRule} per rule.",
                    paramName);
            }
            var seen = new HashSet<int>();
            foreach (var port in rule.Ports)
            {
                if (port < 1 || port > 65535)
                {
                    throw new ArgumentException(
                        $"NetworkRule[{index}] port {port} out of range 1-65535.", paramName);
                }
                if (!seen.Add(port))
                {
                    throw new ArgumentException(
                        $"NetworkRule[{index}] has duplicate port {port}.", paramName);
                }
            }
        }
        if (hasPorts && hasTarget && !IsIpOrCidr(rule.Target!))
        {
            throw new ArgumentException(
                $"NetworkRule[{index}] ports are not supported for domain target '{rule.Target}' " +
                "(domain rules are evaluated at the DNS layer, which has no port dimension).",
                paramName);
        }
    }

    private static Dictionary<string, object?> ToRulesPayloadEntry(NetworkRule r, int index, string? paramName)
    {
        ValidateRule(r, index, paramName);

        // Unset fields are omitted (never null); a blank target degrades to
        // port-only, matching server normalization. Ports are copied so
        // post-validation mutation cannot change the wire.
        var entry = new Dictionary<string, object?>
        {
            ["action"] = r.Action == NetworkRuleAction.Allow ? "allow" : "deny",
        };
        if (!string.IsNullOrWhiteSpace(r.Target))
        {
            entry["target"] = r.Target;
        }
        if (r.Ports is { Count: > 0 })
        {
            var ports = new List<int>(r.Ports.Count);
            foreach (var port in r.Ports)
            {
                ports.Add(port);
            }
            entry["ports"] = ports;
        }
        return entry;
    }

    private const int MaxPortsPerRule = 256;

    /// <summary>
    /// Mirrors the sidecar's netip-based IP-vs-domain classification, on the
    /// trimmed target.
    /// </summary>
    private static bool IsIpOrCidr(string target)
    {
        var t = target.Trim();
        if (t.Count(c => c == '/') > 1)
        {
            return false;
        }
        var slash = t.IndexOf('/');
        var addr = slash < 0 ? t : t[..slash];
        var prefix = slash < 0 ? null : t[(slash + 1)..];
        if (prefix != null)
        {
            if (t.Contains('%') || prefix.Any(c => c is < '0' or > '9'))
            {
                return false;
            }
            var limit = addr.Contains(':') ? 128 : 32;
            if (int.Parse(prefix) > limit)
            {
                return false;
            }
        }
        var host = addr.Contains('%') ? addr[..addr.IndexOf('%')] : addr;
        if (host.Length == 0)
        {
            return false;
        }
        if (addr.Contains(':'))
        {
            return IPAddress.TryParse(host, out _);
        }
        // IPv4: IPAddress.TryParse accepts shorthand ("1" → 0.0.0.1) and
        // leading zeros, so require the dotted-quad form explicitly.
        var octets = host.Split('.');
        return octets.Length == 4 && octets.All(octet =>
            octet.Length is >= 1 and <= 3 &&
            octet.All(c => c is >= '0' and <= '9') &&
            (octet.Length == 1 || octet[0] != '0') &&
            int.Parse(octet) <= 255);
    }

    public static NetworkRule ParseNetworkRule(JsonElement element)
    {
        var actionText = element.GetProperty("action").GetString();

        // The sidecar marshals target without omitempty: a port-only rule
        // arrives as "target": "". Treat blank the same as absent; a
        // non-string, non-null target is contract-invalid.
        string? target = null;
        if (element.TryGetProperty("target", out var targetElement))
        {
            if (targetElement.ValueKind == JsonValueKind.String)
            {
                target = targetElement.GetString();
                if (string.IsNullOrWhiteSpace(target))
                {
                    target = null;
                }
            }
            else if (targetElement.ValueKind != JsonValueKind.Null)
            {
                throw new SandboxApiException(
                    $"Invalid target in network rule: {targetElement.GetRawText()}");
            }
        }

        List<int>? ports = null;
        if (element.TryGetProperty("ports", out var portsElement))
        {
            if (portsElement.ValueKind == JsonValueKind.Array)
            {
                ports = new List<int>();
                foreach (var portElement in portsElement.EnumerateArray())
                {
                    if (portElement.ValueKind != JsonValueKind.Number || !portElement.TryGetInt32(out var port))
                    {
                        throw new SandboxApiException(
                            $"Invalid port element in network rule: {portElement.GetRawText()}");
                    }
                    ports.Add(port);
                }

                if (ports.Count == 0)
                {
                    // The contract declares minItems: 1; accepting an empty
                    // list here would let a GET→PATCH round-trip turn the
                    // rule into "no ports" and trip the write-side checks.
                    throw new SandboxApiException("Invalid network rule: ports must not be empty");
                }
            }
            else if (portsElement.ValueKind != JsonValueKind.Null)
            {
                throw new SandboxApiException(
                    $"Invalid ports in network rule: {portsElement.GetRawText()}");
            }
            // Null ports means "not port-scoped": parsed as absent so a
            // read-back re-PATCHes without a ports key.
        }

        return new NetworkRule
        {
            Action = ParseNetworkRuleAction(actionText),
            Target = target,
            Ports = ports
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
