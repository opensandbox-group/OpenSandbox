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

using System.Text.Json.Serialization;

namespace OpenSandbox.Models;

/// <summary>
/// High-level lifecycle phase of a fsb template build.
/// </summary>
/// <remarks>
/// The server may introduce new phases in future versions; clients should
/// handle unknown string values gracefully.
/// </remarks>
public static class TemplatePhases
{
    /// <summary>
    /// Build accepted, not started yet.
    /// </summary>
    public const string Pending = "Pending";

    /// <summary>
    /// Golden-image build in progress.
    /// </summary>
    public const string Building = "Building";

    /// <summary>
    /// Build finished; the template can back sandbox creation.
    /// </summary>
    public const string Succeeded = "Succeeded";

    /// <summary>
    /// Build failed; see <see cref="TemplateStatus.Message"/>.
    /// </summary>
    public const string Failed = "Failed";
}

/// <summary>
/// Build-side readiness gate for a fsb template.
/// </summary>
public class TemplateReadiness
{
    /// <summary>
    /// Gets or sets the readiness probe checked first during the golden-image build;
    /// e.g. "tcp://127.0.0.1:44772" or "cmd://&lt;command&gt;".
    /// </summary>
    [JsonPropertyName("probe")]
    public string? Probe { get; set; }

    /// <summary>
    /// Gets or sets the fallback warmup window in seconds (default 60).
    /// </summary>
    [JsonPropertyName("warmupSeconds")]
    public int? WarmupSeconds { get; set; }
}

/// <summary>
/// Status of a fsb template build.
/// </summary>
public class TemplateStatus
{
    /// <summary>
    /// Gets or sets the build lifecycle phase (see <see cref="TemplatePhases"/>).
    /// </summary>
    [JsonPropertyName("phase")]
    public required string Phase { get; set; }

    /// <summary>
    /// Gets or sets the S3 manifest reference of the published artifacts;
    /// present when the phase is Succeeded.
    /// </summary>
    [JsonPropertyName("manifestRef")]
    public string? ManifestRef { get; set; }

    /// <summary>
    /// Gets or sets the failure reason when the phase is Failed.
    /// </summary>
    [JsonPropertyName("message")]
    public string? Message { get; set; }
}

/// <summary>
/// Request to create a fsb (fast-sandbox) template: a golden-image build.
/// </summary>
/// <remarks>
/// The build runs asynchronously: the response starts at phase Pending;
/// poll <see cref="Services.ISandboxes.GetTemplateAsync"/> until the phase is
/// Succeeded (or Failed). Kernel, execd and guest init are server-side build
/// inputs, not client fields.
/// </remarks>
public class CreateTemplateRequest
{
    /// <summary>
    /// Gets or sets the source OCI image reference the golden image is built from.
    /// </summary>
    [JsonPropertyName("image")]
    public required string Image { get; set; }

    /// <summary>
    /// Gets or sets the S3-compatible publish target for the built artifacts,
    /// e.g. "s3://bucket/publish".
    /// </summary>
    [JsonPropertyName("publish")]
    public required string Publish { get; set; }

    /// <summary>
    /// Gets or sets the guest machine sizing, e.g. {"cpu": "1", "memory": "512Mi", "disk": "2Gi"}.
    /// Defaults when omitted: cpu "1", memory "512Mi", disk "2Gi".
    /// </summary>
    [JsonPropertyName("resourceLimits")]
    public IReadOnlyDictionary<string, string>? ResourceLimits { get; set; }

    /// <summary>
    /// Gets or sets the guest business command (argv); empty defaults to
    /// ["tail", "-f", "/dev/null"].
    /// </summary>
    [JsonPropertyName("entrypoint")]
    public IReadOnlyList<string>? Entrypoint { get; set; }

    /// <summary>
    /// Gets or sets custom key-value metadata for management, filtering, and tagging.
    /// </summary>
    [JsonPropertyName("metadata")]
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the build-side readiness gate.
    /// </summary>
    [JsonPropertyName("readiness")]
    public TemplateReadiness? Readiness { get; set; }

    /// <summary>
    /// Gets or sets the storage encoding of the produced snapshot set
    /// ("native" or "overlaybd"). Defaults to overlaybd.
    /// </summary>
    [JsonPropertyName("format")]
    public string? Format { get; set; }
}

/// <summary>
/// A fsb template: a golden image whose build is declared and executed by fast-sandbox.
/// </summary>
public class TemplateInfo
{
    /// <summary>
    /// Gets or sets the server-generated template ID ("tpl_&lt;uuid&gt;").
    /// </summary>
    [JsonPropertyName("templateId")]
    public required string TemplateId { get; set; }

    /// <summary>
    /// Gets or sets the source OCI image reference.
    /// </summary>
    [JsonPropertyName("image")]
    public required string Image { get; set; }

    /// <summary>
    /// Gets or sets the S3-compatible publish target.
    /// </summary>
    [JsonPropertyName("publish")]
    public required string Publish { get; set; }

    /// <summary>
    /// Gets or sets the snapshot storage encoding.
    /// </summary>
    [JsonPropertyName("format")]
    public required string Format { get; set; }

    /// <summary>
    /// Gets or sets the current build status.
    /// </summary>
    [JsonPropertyName("status")]
    public required TemplateStatus Status { get; set; }

    /// <summary>
    /// Gets or sets the creation timestamp.
    /// </summary>
    [JsonPropertyName("createdAt")]
    public required DateTime CreatedAt { get; set; }

    /// <summary>
    /// Gets or sets the last update timestamp.
    /// </summary>
    [JsonPropertyName("updatedAt")]
    public required DateTime UpdatedAt { get; set; }

    /// <summary>
    /// Gets or sets the guest machine sizing (cpu/memory/disk).
    /// </summary>
    [JsonPropertyName("resourceLimits")]
    public IReadOnlyDictionary<string, string>? ResourceLimits { get; set; }

    /// <summary>
    /// Gets or sets the guest business command (argv).
    /// </summary>
    [JsonPropertyName("entrypoint")]
    public IReadOnlyList<string>? Entrypoint { get; set; }

    /// <summary>
    /// Gets or sets the custom metadata from the creation request.
    /// </summary>
    [JsonPropertyName("metadata")]
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the build-side readiness gate.
    /// </summary>
    [JsonPropertyName("readiness")]
    public TemplateReadiness? Readiness { get; set; }
}

/// <summary>
/// Response from listing templates.
/// </summary>
public class ListTemplatesResponse
{
    /// <summary>
    /// Gets or sets the list of templates for the current page.
    /// </summary>
    [JsonPropertyName("items")]
    public required IReadOnlyList<TemplateInfo> Items { get; set; }

    /// <summary>
    /// Gets or sets the pagination information.
    /// </summary>
    [JsonPropertyName("pagination")]
    public PaginationInfo? Pagination { get; set; }
}

/// <summary>
/// Parameters for listing templates.
/// </summary>
public class ListTemplatesParams
{
    /// <summary>
    /// Gets or sets the metadata to filter by (AND logic).
    /// </summary>
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the page number (1-indexed).
    /// </summary>
    public int? Page { get; set; }

    /// <summary>
    /// Gets or sets the number of items per page (1-200).
    /// </summary>
    public int? PageSize { get; set; }
}
