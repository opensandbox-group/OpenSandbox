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

/// <summary>
/// Provides weakly consistent command inventory discovery for a sandbox.
/// </summary>
public interface IExecdCommandInventory
{
    /// <summary>
    /// Lists a page of running and terminal command summaries.
    /// </summary>
    /// <param name="running">Filters to running or terminal commands; omit for both.</param>
    /// <param name="limit">Maximum number of command summaries to return.</param>
    /// <param name="cursor">An opaque cursor from a prior page.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A weakly consistent command inventory page.</returns>
    /// <exception cref="Core.InvalidArgumentException">Thrown when <paramref name="limit"/> is outside 1 through 100.</exception>
    /// <exception cref="Core.SandboxException">Thrown when the execd service request fails.</exception>
    Task<ListCommandsPage> ListCommandsAsync(
        bool? running = null,
        int limit = 50,
        string? cursor = null,
        CancellationToken cancellationToken = default);
}
