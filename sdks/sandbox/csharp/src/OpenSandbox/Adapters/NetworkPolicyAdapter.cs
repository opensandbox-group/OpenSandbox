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
using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

/// <summary>
/// Egress policy operations routed through the lifecycle control plane
/// (<c>/sandboxes/{sandboxId}/networkpolicy</c>) instead of the sandbox-side
/// egress sidecar. Used for sandboxes created from fsb templates, where the
/// policy intent is persisted on the Sandbox CR.
/// </summary>
/// <remarks>
/// The lifecycle server exposes the same merge/delete rule semantics as the
/// sidecar policy API (PATCH merges with first-wins-per-target; DELETE removes
/// by target idempotently), so no client-side read-modify-write is needed.
/// </remarks>
internal sealed class NetworkPolicyAdapter : IEgress
{
    private readonly HttpClientWrapper _client;
    private readonly string _sandboxId;

    public NetworkPolicyAdapter(HttpClientWrapper client, string sandboxId)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
        _sandboxId = sandboxId ?? throw new ArgumentNullException(nameof(sandboxId));
    }

    public async Task<NetworkPolicy> GetPolicyAsync(CancellationToken cancellationToken = default)
    {
        var response = await _client.GetAsync<JsonElement>(
            PolicyPath,
            cancellationToken: cancellationToken).ConfigureAwait(false);
        if (!response.TryGetProperty("policy", out var policyElement) || policyElement.ValueKind != JsonValueKind.Object)
        {
            throw new SandboxApiException("Missing policy in network policy response");
        }

        return NetworkPolicyCodec.ParsePolicy(policyElement);
    }

    public async Task PatchRulesAsync(
        IReadOnlyList<NetworkRule> rules,
        CancellationToken cancellationToken = default)
    {
        await _client.PatchAsync(
            PolicyPath,
            NetworkPolicyCodec.ToRulesPayload(rules),
            cancellationToken).ConfigureAwait(false);
    }

    public async Task DeleteRulesAsync(
        IReadOnlyList<string> targets,
        CancellationToken cancellationToken = default)
    {
        await _client.DeleteAsync(
            PolicyPath,
            targets.ToList(),
            cancellationToken).ConfigureAwait(false);
    }

    private string PolicyPath => $"/sandboxes/{Uri.EscapeDataString(_sandboxId)}/networkpolicy";
}
