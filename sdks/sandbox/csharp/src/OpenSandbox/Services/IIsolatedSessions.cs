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

using OpenSandbox.Models;

namespace OpenSandbox.Services;

public interface IIsolationSession
{
    string SessionId { get; }
    IsolatedSessionInfo Info { get; }
    ISandboxFiles Files { get; }

    Task<Execution> RunAsync(
        string code,
        IsolatedRunOpts? opts = null,
        ExecutionHandlers? handlers = null,
        CancellationToken cancellationToken = default);

    Task<IsolatedSessionState> GetAsync(
        CancellationToken cancellationToken = default);

    Task DeleteAsync(
        CancellationToken cancellationToken = default);
}

public interface IIsolatedSessions
{
    Task<IIsolationSession> CreateAsync(
        CreateIsolatedSessionRequest request,
        CancellationToken cancellationToken = default);

    Task<IsolatedCapabilities> CapabilitiesAsync(
        CancellationToken cancellationToken = default);

    Task<IReadOnlyList<IsolatedSessionSummary>> ListAsync(
        CancellationToken cancellationToken = default);
}
