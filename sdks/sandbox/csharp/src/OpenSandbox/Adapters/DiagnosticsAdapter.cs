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

internal sealed class DiagnosticsAdapter(HttpClientWrapper client) : IDiagnostics
{
    public Task<DiagnosticContext> GetContextAsync(string sandboxId, CancellationToken cancellationToken = default) =>
        client.GetAsync<DiagnosticContext>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/diagnostics/context",
            cancellationToken: cancellationToken);

    public Task<DiagnosticContent> GetLogsAsync(string sandboxId, string scope, CancellationToken cancellationToken = default) =>
        GetContentAsync(sandboxId, scope, "logs", cancellationToken);

    public Task<DiagnosticContent> GetEventsAsync(string sandboxId, string scope, CancellationToken cancellationToken = default) =>
        GetContentAsync(sandboxId, scope, "events", cancellationToken);

    private Task<DiagnosticContent> GetContentAsync(string sandboxId, string scope, string kind, CancellationToken cancellationToken) =>
        client.GetAsync<DiagnosticContent>(
            $"/sandboxes/{Uri.EscapeDataString(sandboxId)}/diagnostics/{kind}",
            new Dictionary<string, string?> { ["scope"] = scope },
            cancellationToken);
}
