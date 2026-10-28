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

namespace OpenSandbox.Models;

/// <summary>Backend routing context, independent of execd readiness.</summary>
public class DiagnosticContext
{
    /// <summary>Gets or sets the sandbox identifier.</summary>
    public required string SandboxId { get; set; }
    /// <summary>Gets or sets the current lifecycle state.</summary>
    public string? State { get; set; }
    /// <summary>Gets or sets the backend owning the sandbox.</summary>
    public required string BackendType { get; set; }
    /// <summary>Gets or sets backend-defined diagnostic identifiers.</summary>
    public required Dictionary<string, string> BackendIdentifiers { get; set; }
}

/// <summary>Diagnostic text descriptor. URL content is not downloaded automatically.</summary>
public class DiagnosticContent
{
    /// <summary>Gets or sets the sandbox identifier.</summary>
    public required string SandboxId { get; set; }
    /// <summary>Gets or sets the kind: logs or events.</summary>
    public required string Kind { get; set; }
    /// <summary>Gets or sets the selected scope.</summary>
    public required string Scope { get; set; }
    /// <summary>Gets or sets the delivery mode: inline or url.</summary>
    public required string Delivery { get; set; }
    /// <summary>Gets or sets the text's media type.</summary>
    public required string ContentType { get; set; }
    /// <summary>Gets or sets whether diagnostic text was truncated.</summary>
    public required bool Truncated { get; set; }
    /// <summary>Gets or sets the inline text, including empty text.</summary>
    public string? Content { get; set; }
    /// <summary>Gets or sets the download URL.</summary>
    public string? ContentUrl { get; set; }
    /// <summary>Gets or sets the text length in bytes.</summary>
    public long? ContentLength { get; set; }
    /// <summary>Gets or sets the download URL's expiration.</summary>
    public DateTimeOffset? ExpiresAt { get; set; }
    /// <summary>Gets or sets warnings about diagnostic completeness.</summary>
    public List<string>? Warnings { get; set; }
}
