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

using OpenSandbox.Core;

namespace OpenSandbox.Services;

/// <summary>
/// Service interface for health checks on the execd service.
/// </summary>
public interface IExecdHealth
{
    /// <summary>
    /// Pings the execd service to check if it's healthy.
    /// </summary>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>True if the service is healthy, false otherwise.</returns>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<bool> PingAsync(CancellationToken cancellationToken = default);
}
