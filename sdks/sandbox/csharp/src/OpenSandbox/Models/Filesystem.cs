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

using System.Text.Json.Serialization;

namespace OpenSandbox.Models;

/// <summary>
/// Information about a file in the sandbox.
/// </summary>
public class SandboxFileInfo
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    [JsonPropertyName("path")]
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the file size in bytes.
    /// </summary>
    [JsonPropertyName("size")]
    public long? Size { get; set; }

    /// <summary>
    /// Gets or sets the last modification time.
    /// </summary>
    [JsonPropertyName("modified_at")]
    public DateTime? ModifiedAt { get; set; }

    /// <summary>
    /// Gets or sets the creation time.
    /// </summary>
    [JsonPropertyName("created_at")]
    public DateTime? CreatedAt { get; set; }

    /// <summary>
    /// Gets or sets the file mode (permissions).
    /// </summary>
    [JsonPropertyName("mode")]
    public int? Mode { get; set; }

    /// <summary>
    /// Gets or sets the file owner.
    /// </summary>
    [JsonPropertyName("owner")]
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the file group.
    /// </summary>
    [JsonPropertyName("group")]
    public string? Group { get; set; }
}

/// <summary>
/// File permission settings.
/// </summary>
public class Permission
{
    /// <summary>
    /// Gets or sets the file mode (permissions).
    /// </summary>
    [JsonPropertyName("mode")]
    public int Mode { get; set; }

    /// <summary>
    /// Gets or sets the file owner.
    /// </summary>
    [JsonPropertyName("owner")]
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the file group.
    /// </summary>
    [JsonPropertyName("group")]
    public string? Group { get; set; }
}

/// <summary>
/// File metadata for upload operations.
/// </summary>
public class FileMetadata
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    [JsonPropertyName("path")]
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the file mode (permissions).
    /// </summary>
    [JsonPropertyName("mode")]
    public int? Mode { get; set; }

    /// <summary>
    /// Gets or sets the file owner.
    /// </summary>
    [JsonPropertyName("owner")]
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the file group.
    /// </summary>
    [JsonPropertyName("group")]
    public string? Group { get; set; }
}

/// <summary>
/// Entry for writing a file.
/// </summary>
public class WriteEntry
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the file data.
    /// Supports: string, byte[], Stream.
    /// </summary>
    public object? Data { get; set; }

    /// <summary>
    /// Gets or sets the file mode (permissions).
    /// </summary>
    public int? Mode { get; set; }

    /// <summary>
    /// Gets or sets the file owner.
    /// </summary>
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the file group.
    /// </summary>
    public string? Group { get; set; }
}

/// <summary>
/// Entry for creating a directory.
/// </summary>
public class CreateDirectoryEntry
{
    /// <summary>
    /// Gets or sets the directory path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the directory mode (permissions).
    /// </summary>
    public int? Mode { get; set; }

    /// <summary>
    /// Gets or sets the directory owner.
    /// </summary>
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the directory group.
    /// </summary>
    public string? Group { get; set; }
}

/// <summary>
/// Entry for searching files.
/// </summary>
public class SearchEntry
{
    /// <summary>
    /// Gets or sets the search path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the search pattern (e.g., "*.txt").
    /// </summary>
    public string? Pattern { get; set; }
}

/// <summary>
/// Entry for moving/renaming a file.
/// </summary>
public class MoveEntry
{
    /// <summary>
    /// Gets or sets the source path.
    /// </summary>
    public required string Src { get; set; }

    /// <summary>
    /// Gets or sets the destination path.
    /// </summary>
    public required string Dest { get; set; }
}

/// <summary>
/// Entry for replacing content in a file.
/// </summary>
public class ContentReplaceEntry
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the old content to replace.
    /// </summary>
    public required string OldContent { get; set; }

    /// <summary>
    /// Gets or sets the new content.
    /// </summary>
    public required string NewContent { get; set; }
}

/// <summary>
/// Entry for setting file permissions.
/// </summary>
public class SetPermissionEntry
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the file mode (permissions).
    /// </summary>
    public required int Mode { get; set; }

    /// <summary>
    /// Gets or sets the file owner.
    /// </summary>
    public string? Owner { get; set; }

    /// <summary>
    /// Gets or sets the file group.
    /// </summary>
    public string? Group { get; set; }
}

/// <summary>
/// Options for reading a file as text.
/// </summary>
public class ReadFileOptions
{
    /// <summary>
    /// Gets or sets the text encoding (default: utf-8).
    /// </summary>
    public string? Encoding { get; set; }

    /// <summary>
    /// Gets or sets the byte range to read (e.g., "bytes=0-1023").
    /// </summary>
    public string? Range { get; set; }
}

/// <summary>
/// Options for reading a file as bytes.
/// </summary>
public class ReadBytesOptions
{
    /// <summary>
    /// Gets or sets the byte range to read (e.g., "bytes=0-1023").
    /// </summary>
    public string? Range { get; set; }
}

/// <summary>
/// API request model for renaming files.
/// </summary>
public class RenameFileItem
{
    /// <summary>
    /// Gets or sets the source path.
    /// </summary>
    [JsonPropertyName("src")]
    public required string Src { get; set; }

    /// <summary>
    /// Gets or sets the destination path.
    /// </summary>
    [JsonPropertyName("dest")]
    public required string Dest { get; set; }
}

/// <summary>
/// API request model for replacing file content.
/// </summary>
public class ReplaceFileContentItem
{
    /// <summary>
    /// Gets or sets the old content to replace.
    /// </summary>
    [JsonPropertyName("old")]
    public required string Old { get; set; }

    /// <summary>
    /// Gets or sets the new content.
    /// </summary>
    [JsonPropertyName("new")]
    public required string New { get; set; }
}

/// <summary>
/// Result of a content replacement operation on a single file.
/// </summary>
public class ContentReplaceResult
{
    /// <summary>
    /// Gets or sets the file path.
    /// </summary>
    public required string Path { get; set; }

    /// <summary>
    /// Gets or sets the number of occurrences replaced. 0 means old content was not found.
    /// </summary>
    public int ReplacedCount { get; set; }
}

/// <summary>
/// API response model for replace file content result.
/// </summary>
public class ReplaceFileContentResult
{
    /// <summary>
    /// Gets or sets the number of occurrences replaced.
    /// </summary>
    [JsonPropertyName("replacedCount")]
    public int ReplacedCount { get; set; }
}
