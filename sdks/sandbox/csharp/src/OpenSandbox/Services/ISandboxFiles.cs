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
using OpenSandbox.Core;

namespace OpenSandbox.Services;

/// <summary>
/// Service interface for filesystem operations in a sandbox.
/// </summary>
public interface ISandboxFiles
{
    /// <summary>
    /// Gets information about files at the specified paths.
    /// </summary>
    /// <param name="paths">The file paths to query.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A dictionary mapping paths to file information.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<IReadOnlyDictionary<string, SandboxFileInfo>> GetFileInfoAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Searches for files matching the specified criteria.
    /// </summary>
    /// <param name="entry">The search entry with path and pattern.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A list of matching files.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<IReadOnlyList<SandboxFileInfo>> SearchAsync(
        SearchEntry entry,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Creates directories at the specified paths.
    /// </summary>
    /// <param name="entries">The directory entries to create.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task CreateDirectoriesAsync(
        IEnumerable<CreateDirectoryEntry> entries,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes directories at the specified paths.
    /// </summary>
    /// <param name="paths">The directory paths to delete.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task DeleteDirectoriesAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Lists directory contents with optional depth control.
    /// </summary>
    /// <param name="path">The directory path to list.</param>
    /// <param name="depth">Optional maximum child depth to include.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>A list of directory entries with metadata.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<IReadOnlyList<SandboxFileInfo>> ListDirectoryAsync(
        string path,
        int? depth = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Writes files to the sandbox.
    /// </summary>
    /// <param name="entries">The file entries to write.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task WriteFilesAsync(
        IEnumerable<WriteEntry> entries,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Reads a file as text.
    /// </summary>
    /// <param name="path">The file path.</param>
    /// <param name="options">Optional read options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The file content as a string.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<string> ReadFileAsync(
        string path,
        ReadFileOptions? options = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Reads a file as bytes.
    /// </summary>
    /// <param name="path">The file path.</param>
    /// <param name="options">Optional read options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>The file content as a byte array.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<byte[]> ReadBytesAsync(
        string path,
        ReadBytesOptions? options = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Reads a file as a stream of byte chunks.
    /// </summary>
    /// <param name="path">The file path.</param>
    /// <param name="options">Optional read options.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>An async enumerable of byte chunks.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    IAsyncEnumerable<byte[]> ReadBytesStreamAsync(
        string path,
        ReadBytesOptions? options = null,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes files at the specified paths.
    /// </summary>
    /// <param name="paths">The file paths to delete.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task DeleteFilesAsync(
        IEnumerable<string> paths,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Moves/renames files.
    /// </summary>
    /// <param name="entries">The move entries with source and destination paths.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task MoveFilesAsync(
        IEnumerable<MoveEntry> entries,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Replaces content in files.
    /// </summary>
    /// <param name="entries">The content replace entries.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task ReplaceContentsAsync(
        IEnumerable<ContentReplaceEntry> entries,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Replaces content in files and returns per-file replacement counts.
    /// </summary>
    /// <param name="entries">The content replace entries.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <returns>List of replacement results with counts per file.</returns>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task<IReadOnlyList<ContentReplaceResult>> ReplaceContentsDetailedAsync(
        IEnumerable<ContentReplaceEntry> entries,
        CancellationToken cancellationToken = default);

    /// <summary>
    /// Sets permissions on files.
    /// </summary>
    /// <param name="entries">The permission entries.</param>
    /// <param name="cancellationToken">Cancellation token.</param>
    /// <exception cref="InvalidArgumentException">Thrown when request values are invalid.</exception>
    /// <exception cref="SandboxException">Thrown when the execd service request fails.</exception>
    Task SetPermissionsAsync(
        IEnumerable<SetPermissionEntry> entries,
        CancellationToken cancellationToken = default);
}
