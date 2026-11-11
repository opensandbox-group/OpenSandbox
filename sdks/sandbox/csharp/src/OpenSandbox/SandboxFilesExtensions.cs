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
using OpenSandbox.Services;

namespace OpenSandbox;

/// <summary>Optional download metadata operations on filesystem adapters.</summary>
public static class SandboxFilesExtensions
{
    /// <summary>Reads file bytes and HTTP response metadata.</summary>
    /// <exception cref="NotSupportedException">The adapter does not support download metadata.</exception>
    public static Task<ReadBytesResponse<byte[]>> ReadBytesDetailedAsync(
        this ISandboxFiles files,
        string path,
        ReadBytesOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        if (files is not IDownloadSandboxFiles downloads)
            throw new NotSupportedException("The selected filesystem adapter does not support download metadata.");
        return downloads.ReadBytesDetailedAsync(path, options, cancellationToken);
    }

    /// <summary>Opens a byte stream with HTTP response metadata; dispose the body after use.</summary>
    /// <exception cref="NotSupportedException">The adapter does not support download metadata.</exception>
    public static Task<ReadBytesResponse<IAsyncReadBytesStream>> ReadBytesStreamDetailedAsync(
        this ISandboxFiles files,
        string path,
        ReadBytesOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        if (files is not IDownloadSandboxFiles downloads)
            throw new NotSupportedException("The selected filesystem adapter does not support download metadata.");
        return downloads.ReadBytesStreamDetailedAsync(path, options, cancellationToken);
    }
}
