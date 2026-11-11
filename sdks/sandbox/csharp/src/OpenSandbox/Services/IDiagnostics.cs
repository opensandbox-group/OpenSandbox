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

using OpenSandbox.Models;

namespace OpenSandbox.Services;

/// <summary>Lifecycle-server diagnostics, independent of execd readiness.</summary>
public interface IDiagnostics
{
    /// <summary>Gets backend routing context.</summary>
    Task<DiagnosticContext> GetContextAsync(string sandboxId, CancellationToken cancellationToken = default);
    /// <summary>Gets a scoped log descriptor.</summary>
    Task<DiagnosticContent> GetLogsAsync(string sandboxId, string scope, CancellationToken cancellationToken = default);
    /// <summary>Gets a scoped event descriptor.</summary>
    Task<DiagnosticContent> GetEventsAsync(string sandboxId, string scope, CancellationToken cancellationToken = default);
}
