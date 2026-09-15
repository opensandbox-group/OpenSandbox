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

using System.Collections.Generic;
using System.Text.Json;
using System.Linq;
using OpenSandbox.Core;
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

internal sealed class EgressAdapter : IEgress, ICredentialVault
{
    private readonly HttpClientWrapper _client;

    public EgressAdapter(HttpClientWrapper client)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
    }

    public async Task<CredentialVaultState> CreateAsync(
        IReadOnlyList<Credential> credentials,
        IReadOnlyList<CredentialBinding> bindings,
        CancellationToken cancellationToken = default)
    {
        var request = new CredentialVaultCreateRequest
        {
            Credentials = credentials,
            Bindings = bindings
        };

        return await _client.PostAsync<CredentialVaultState>(
            "/credential-vault",
            request,
            cancellationToken).ConfigureAwait(false);
    }

    public async Task<CredentialVaultState> GetAsync(CancellationToken cancellationToken = default)
    {
        return await _client.GetAsync<CredentialVaultState>(
            "/credential-vault",
            cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<CredentialVaultState> PatchAsync(
        CredentialVaultPatchRequest request,
        CancellationToken cancellationToken = default)
    {
        if (request == null)
        {
            throw new ArgumentNullException(nameof(request));
        }

        return await _client.PatchAsync<CredentialVaultState>(
            "/credential-vault",
            request,
            cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteAsync(CancellationToken cancellationToken = default)
    {
        await _client.DeleteAsync("/credential-vault", cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<CredentialMetadata>> ListCredentialsAsync(
        CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<CredentialListResponse>(
            "/credential-vault/credentials",
            cancellationToken: cancellationToken).ConfigureAwait(false);

        return response.Credentials;
    }

    public async Task<CredentialMetadata> GetCredentialAsync(
        string name,
        CancellationToken cancellationToken = default)
    {
        return await _client.GetAsync<CredentialMetadata>(
            $"/credential-vault/credentials/{EncodePathSegment(name)}",
            cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<CredentialBindingMetadata>> ListBindingsAsync(
        CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<CredentialBindingListResponse>(
            "/credential-vault/bindings",
            cancellationToken: cancellationToken).ConfigureAwait(false);

        return response.Bindings;
    }

    public async Task<CredentialBindingMetadata> GetBindingAsync(
        string name,
        CancellationToken cancellationToken = default)
    {
        return await _client.GetAsync<CredentialBindingMetadata>(
            $"/credential-vault/bindings/{EncodePathSegment(name)}",
            cancellationToken: cancellationToken).ConfigureAwait(false);
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
        // target/ports are omitted (not sent as null) when unset, matching the
        // wire shape the sidecar/other SDKs produce for a port-only rule.
        var normalizedRules = rules.Select(r =>
        {
            var normalized = new Dictionary<string, object?>
            {
                ["action"] = r.Action == NetworkRuleAction.Allow ? "allow" : "deny",
            };
            if (r.Target is not null)
            {
                normalized["target"] = r.Target;
            }
            if (r.Ports is { Count: > 0 })
            {
                normalized["ports"] = r.Ports;
            }
            return normalized;
        }).ToList();

        await _client.PatchAsync("/policy", normalizedRules, cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteRulesAsync(
        IReadOnlyList<string> targets,
        CancellationToken cancellationToken = default)
    {
        await _client.DeleteAsync("/policy", targets.ToList(), cancellationToken).ConfigureAwait(false);
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

        // target is absent (not merely empty) for a port-only rule; GetProperty
        // would throw KeyNotFoundException in that case, so use TryGetProperty.
        string? target = null;
        if (element.TryGetProperty("target", out var targetElement) &&
            targetElement.ValueKind == JsonValueKind.String)
        {
            target = targetElement.GetString();
        }

        List<int>? ports = null;
        if (element.TryGetProperty("ports", out var portsElement) &&
            portsElement.ValueKind == JsonValueKind.Array)
        {
            ports = portsElement.EnumerateArray().Select(p => p.GetInt32()).ToList();
        }

        return new NetworkRule
        {
            Action = ParseNetworkRuleAction(actionText),
            Target = target,
            Ports = ports
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

    private static string EncodePathSegment(string value)
    {
        if (value == null)
        {
            throw new ArgumentNullException(nameof(value));
        }

        return Uri.EscapeDataString(value);
    }
}
