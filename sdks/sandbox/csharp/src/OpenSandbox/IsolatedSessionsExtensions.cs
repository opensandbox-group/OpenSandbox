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
using OpenSandbox.Services;

namespace OpenSandbox;

public static class IsolatedSessionsExtensions
{
    public static async Task<Execution> RunOnceAsync(
        this IIsolatedSessions service,
        string code,
        string workspace,
        string? workspaceMode = null,
        IsolatedRunOpts? opts = null,
        ExecutionHandlers? handlers = null,
        string? profile = null,
        bool? shareNet = null,
        List<BindMount>? binds = null,
        CancellationToken cancellationToken = default)
    {
        var request = new CreateIsolatedSessionRequest(
            Workspace: new IsolatedWorkspaceSpec(Path: workspace, Mode: workspaceMode),
            Profile: profile,
            ShareNet: shareNet,
            Binds: binds
        );
        var session = await service.CreateAsync(request, cancellationToken).ConfigureAwait(false);
        try
        {
            return await session.RunAsync(code, opts, handlers, cancellationToken).ConfigureAwait(false);
        }
        finally
        {
            try
            {
                await session.DeleteAsync(CancellationToken.None).ConfigureAwait(false);
            }
            catch
            {
                // best-effort cleanup
            }
        }
    }

    public static async Task<T> WithSessionAsync<T>(
        this IIsolatedSessions service,
        CreateIsolatedSessionRequest request,
        Func<IIsolationSession, Task<T>> fn,
        CancellationToken cancellationToken = default)
    {
        var session = await service.CreateAsync(request, cancellationToken).ConfigureAwait(false);
        try
        {
            return await fn(session).ConfigureAwait(false);
        }
        finally
        {
            try
            {
                await session.DeleteAsync(CancellationToken.None).ConfigureAwait(false);
            }
            catch
            {
                // best-effort cleanup
            }
        }
    }

    public static async Task WithSessionAsync(
        this IIsolatedSessions service,
        CreateIsolatedSessionRequest request,
        Func<IIsolationSession, Task> fn,
        CancellationToken cancellationToken = default)
    {
        var session = await service.CreateAsync(request, cancellationToken).ConfigureAwait(false);
        try
        {
            await fn(session).ConfigureAwait(false);
        }
        finally
        {
            try
            {
                await session.DeleteAsync(CancellationToken.None).ConfigureAwait(false);
            }
            catch
            {
                // best-effort cleanup
            }
        }
    }
}
