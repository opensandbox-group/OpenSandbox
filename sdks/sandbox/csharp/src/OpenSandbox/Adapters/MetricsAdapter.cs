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

using OpenSandbox.Internal;
using OpenSandbox.Models;
using OpenSandbox.Services;

namespace OpenSandbox.Adapters;

/// <summary>
/// Adapter for the execd metrics service.
/// </summary>
internal sealed class MetricsAdapter : IExecdMetrics
{
    private readonly HttpClientWrapper _client;

    public MetricsAdapter(HttpClientWrapper client)
    {
        _client = client ?? throw new ArgumentNullException(nameof(client));
    }

    public async Task<SandboxMetrics> GetMetricsAsync(CancellationToken cancellationToken = default)
    {
        var metrics = await _client.GetAsync<Metrics>("/metrics", cancellationToken: cancellationToken).ConfigureAwait(false);
        return NormalizeMetrics(metrics);
    }

    private static SandboxMetrics NormalizeMetrics(Metrics m)
    {
        return new SandboxMetrics
        {
            CpuCount = m.CpuCount ?? 0,
            CpuUsedPercentage = m.CpuUsedPct ?? 0,
            MemoryTotalMiB = m.MemTotalMib ?? 0,
            MemoryUsedMiB = m.MemUsedMib ?? 0,
            Timestamp = m.Timestamp ?? 0
        };
    }
}
