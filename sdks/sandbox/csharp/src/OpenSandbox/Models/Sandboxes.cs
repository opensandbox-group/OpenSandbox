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
/// Authentication credentials for pulling container images.
/// </summary>
public class ImageAuth
{
    /// <summary>
    /// Gets or sets the username for authentication.
    /// </summary>
    [JsonPropertyName("username")]
    public string? Username { get; set; }

    /// <summary>
    /// Gets or sets the password for authentication.
    /// </summary>
    [JsonPropertyName("password")]
    public string? Password { get; set; }

    /// <summary>
    /// Gets or sets the token for authentication.
    /// </summary>
    [JsonPropertyName("token")]
    public string? Token { get; set; }
}

/// <summary>
/// Specification for a container image.
/// </summary>
public class ImageSpec
{
    /// <summary>
    /// Gets or sets the image URI (e.g., "python:3.11").
    /// </summary>
    [JsonPropertyName("uri")]
    public required string Uri { get; set; }

    /// <summary>
    /// Gets or sets the optional authentication credentials.
    /// </summary>
    [JsonPropertyName("auth")]
    public ImageAuth? Auth { get; set; }
}

/// <summary>
/// Runtime platform constraint for sandbox provisioning.
/// </summary>
public class PlatformSpec
{
    /// <summary>
    /// Gets or sets the target operating system.
    /// </summary>
    [JsonPropertyName("os")]
    public required string Os { get; set; }

    /// <summary>
    /// Gets or sets the target CPU architecture.
    /// </summary>
    [JsonPropertyName("arch")]
    public required string Arch { get; set; }
}

/// <summary>
/// Action for a network rule.
/// </summary>
[JsonConverter(typeof(JsonStringEnumConverter))]
public enum NetworkRuleAction
{
    /// <summary>
    /// Allow the network traffic.
    /// </summary>
    [JsonPropertyName("allow")]
    Allow,

    /// <summary>
    /// Deny the network traffic.
    /// </summary>
    [JsonPropertyName("deny")]
    Deny
}

/// <summary>
/// A network rule for egress traffic.
/// </summary>
public class NetworkRule
{
    /// <summary>
    /// Gets or sets whether to allow or deny matching targets.
    /// </summary>
    [JsonPropertyName("action")]
    public required NetworkRuleAction Action { get; set; }

    /// <summary>
    /// Gets or sets the FQDN or wildcard domain (e.g., "example.com", "*.example.com").
    /// </summary>
    [JsonPropertyName("target")]
    public required string Target { get; set; }
}

/// <summary>
/// Network policy for sandbox egress traffic.
/// </summary>
public class NetworkPolicy
{
    /// <summary>
    /// Gets or sets the default action when no egress rule matches. Defaults to "deny".
    /// </summary>
    [JsonPropertyName("defaultAction")]
    public NetworkRuleAction? DefaultAction { get; set; }

    /// <summary>
    /// Gets or sets the list of egress rules evaluated in order.
    /// </summary>
    [JsonPropertyName("egress")]
    public List<NetworkRule>? Egress { get; set; }
}

/// <summary>
/// Host path bind mount backend for a volume.
/// </summary>
public class Host
{
    /// <summary>
    /// Gets or sets the absolute host path.
    /// Must start with '/' (Unix) or a drive letter such as 'C:\' or 'D:/'
    /// (Windows), and be under an allowed prefix.
    /// </summary>
    [JsonPropertyName("path")]
    public required string Path { get; set; }
}

/// <summary>
/// Platform-managed named volume backend (PVC in k8s, named volume in Docker).
/// </summary>
public class PVC
{
    /// <summary>
    /// Gets or sets the target claim/volume name.
    /// </summary>
    [JsonPropertyName("claimName")]
    public required string ClaimName { get; set; }

    /// <summary>
    /// Gets or sets whether to auto-create the volume if it does not exist. Defaults to true.
    /// </summary>
    [JsonPropertyName("createIfNotExists")]
    public bool? CreateIfNotExists { get; set; }

    /// <summary>
    /// Gets or sets whether the auto-created volume (Docker named volume or Kubernetes PVC)
    /// should be removed on sandbox deletion. Pre-existing volumes are never removed.
    /// </summary>
    [JsonPropertyName("deleteOnSandboxTermination")]
    public bool? DeleteOnSandboxTermination { get; set; }

    /// <summary>
    /// Gets or sets the Kubernetes StorageClass for auto-created PVCs. Ignored for Docker.
    /// </summary>
    [JsonPropertyName("storageClass")]
    public string? StorageClass { get; set; }

    /// <summary>
    /// Gets or sets the storage request for auto-created PVCs (e.g. "1Gi"). Ignored for Docker.
    /// </summary>
    [JsonPropertyName("storage")]
    public string? Storage { get; set; }

    /// <summary>
    /// Gets or sets access modes for auto-created PVCs (e.g. "ReadWriteOnce"). Ignored for Docker.
    /// </summary>
    [JsonPropertyName("accessModes")]
    public IReadOnlyList<string>? AccessModes { get; set; }
}

/// <summary>
/// Alibaba Cloud OSS mount backend via ossfs.
/// </summary>
public class OSSFS
{
    /// <summary>
    /// Gets or sets the OSS bucket name.
    /// </summary>
    [JsonPropertyName("bucket")]
    public required string Bucket { get; set; }

    /// <summary>
    /// Gets or sets the OSS endpoint.
    /// </summary>
    [JsonPropertyName("endpoint")]
    public required string Endpoint { get; set; }

    /// <summary>
    /// Gets or sets the OSS access key ID for inline credentials mode.
    /// </summary>
    [JsonPropertyName("accessKeyId")]
    public required string AccessKeyId { get; set; }

    /// <summary>
    /// Gets or sets the OSS access key secret for inline credentials mode.
    /// </summary>
    [JsonPropertyName("accessKeySecret")]
    public required string AccessKeySecret { get; set; }

    /// <summary>
    /// Gets or sets the ossfs major version used by runtime mount integration. Defaults to "2.0".
    /// </summary>
    [JsonPropertyName("version")]
    public string Version { get; set; } = "2.0";

    /// <summary>
    /// Gets or sets additional ossfs mount options.
    /// </summary>
    [JsonPropertyName("options")]
    public IReadOnlyList<string>? Options { get; set; }
}

/// <summary>
/// Storage mount definition for sandbox creation.
/// Exactly one backend (Host, PVC, or OSSFS) should be provided per volume.
/// </summary>
public class Volume
{
    /// <summary>
    /// Gets or sets the unique volume name within this sandbox request.
    /// </summary>
    [JsonPropertyName("name")]
    public required string Name { get; set; }

    /// <summary>
    /// Gets or sets the host-path backend configuration.
    /// </summary>
    [JsonPropertyName("host")]
    public Host? Host { get; set; }

    /// <summary>
    /// Gets or sets the PVC/named-volume backend configuration.
    /// </summary>
    [JsonPropertyName("pvc")]
    public PVC? Pvc { get; set; }

    /// <summary>
    /// Gets or sets the OSSFS backend configuration.
    /// </summary>
    [JsonPropertyName("ossfs")]
    public OSSFS? Ossfs { get; set; }

    /// <summary>
    /// Gets or sets the absolute mount path inside the container.
    /// </summary>
    [JsonPropertyName("mountPath")]
    public required string MountPath { get; set; }

    /// <summary>
    /// Gets or sets whether this volume is mounted read-only.
    /// </summary>
    [JsonPropertyName("readOnly")]
    public bool? ReadOnly { get; set; }

    /// <summary>
    /// Gets or sets the optional relative subpath under the volume backend.
    /// </summary>
    [JsonPropertyName("subPath")]
    public string? SubPath { get; set; }
}

/// <summary>
/// Status of a sandbox.
/// </summary>
public class SandboxStatus
{
    /// <summary>
    /// Gets or sets the current state of the sandbox.
    /// </summary>
    [JsonPropertyName("state")]
    public required string State { get; set; }

    /// <summary>
    /// Gets or sets the reason for the current state.
    /// </summary>
    [JsonPropertyName("reason")]
    public string? Reason { get; set; }

    /// <summary>
    /// Gets or sets additional message about the current state.
    /// </summary>
    [JsonPropertyName("message")]
    public string? Message { get; set; }
}

/// <summary>
/// Information about a sandbox.
/// </summary>
public class SandboxInfo
{
    /// <summary>
    /// Gets or sets the sandbox ID.
    /// </summary>
    [JsonPropertyName("id")]
    public required string Id { get; set; }

    /// <summary>
    /// Gets or sets the container image specification.
    /// </summary>
    [JsonPropertyName("image")]
    public ImageSpec? Image { get; set; }

    /// <summary>
    /// Gets or sets the snapshot identifier used to restore this sandbox.
    /// </summary>
    [JsonPropertyName("snapshotId")]
    public string? SnapshotId { get; set; }

    /// <summary>
    /// Gets or sets the entrypoint command.
    /// </summary>
    [JsonPropertyName("entrypoint")]
    public required IReadOnlyList<string> Entrypoint { get; set; }

    /// <summary>
    /// Gets or sets the custom metadata tags.
    /// </summary>
    [JsonPropertyName("metadata")]
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the sandbox status.
    /// </summary>
    [JsonPropertyName("status")]
    public required SandboxStatus Status { get; set; }

    /// <summary>
    /// Gets or sets the effective platform used for sandbox provisioning.
    /// </summary>
    [JsonPropertyName("platform")]
    public PlatformSpec? Platform { get; set; }

    /// <summary>
    /// Gets or sets the sandbox creation time.
    /// </summary>
    [JsonPropertyName("createdAt")]
    public required DateTime CreatedAt { get; set; }

    /// <summary>
    /// Gets or sets the sandbox expiration time.
    /// </summary>
    [JsonPropertyName("expiresAt")]
    public DateTime? ExpiresAt { get; set; }
}

/// <summary>
/// Metadata merge patch for a sandbox. Non-null values add or replace keys; null values delete keys.
/// </summary>
public class SandboxMetadataPatch : Dictionary<string, string?>
{
    public SandboxMetadataPatch()
    {
    }

    public SandboxMetadataPatch(IDictionary<string, string?> dictionary) : base(dictionary)
    {
    }
}

/// <summary>
/// Request to create a new sandbox.
/// </summary>
public class CreateSandboxRequest
{
    /// <summary>
    /// Gets or sets the container image specification.
    /// </summary>
    [JsonPropertyName("image")]
    public ImageSpec? Image { get; set; }

    /// <summary>
    /// Gets or sets the snapshot identifier to restore from.
    /// </summary>
    [JsonPropertyName("snapshotId")]
    public string? SnapshotId { get; set; }

    /// <summary>
    /// Gets or sets the entrypoint command.
    /// </summary>
    [JsonPropertyName("entrypoint")]
    public IReadOnlyList<string>? Entrypoint { get; set; }

    /// <summary>
    /// Gets or sets the timeout in seconds.
    /// </summary>
    [JsonPropertyName("timeout")]
    public int? Timeout { get; set; }

    /// <summary>
    /// Gets or sets the resource limits.
    /// </summary>
    [JsonPropertyName("resourceLimits")]
    public required IReadOnlyDictionary<string, string> ResourceLimits { get; set; }

    /// <summary>
    /// Gets or sets the environment variables.
    /// </summary>
    [JsonPropertyName("env")]
    public IReadOnlyDictionary<string, string>? Env { get; set; }

    /// <summary>
    /// Gets or sets whether to enable secured access for sandbox endpoints.
    /// </summary>
    [JsonPropertyName("secureAccess")]
    public bool? SecureAccess { get; set; }

    /// <summary>
    /// Gets or sets the custom metadata tags.
    /// </summary>
    [JsonPropertyName("metadata")]
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the network policy.
    /// </summary>
    [JsonPropertyName("networkPolicy")]
    public NetworkPolicy? NetworkPolicy { get; set; }

    /// <summary>
    /// Gets or sets an optional platform constraint for sandbox provisioning.
    /// </summary>
    [JsonPropertyName("platform")]
    public PlatformSpec? Platform { get; set; }

    /// <summary>
    /// Gets or sets storage volumes to mount into the sandbox.
    /// </summary>
    [JsonPropertyName("volumes")]
    public IReadOnlyList<Volume>? Volumes { get; set; }

    /// <summary>
    /// Gets or sets the extension parameters.
    /// </summary>
    [JsonPropertyName("extensions")]
    public IReadOnlyDictionary<string, object>? Extensions { get; set; }
}

/// <summary>
/// Response from creating a sandbox.
/// </summary>
public class CreateSandboxResponse
{
    /// <summary>
    /// Gets or sets the sandbox ID.
    /// </summary>
    [JsonPropertyName("id")]
    public required string Id { get; set; }

    /// <summary>
    /// Gets or sets the sandbox status.
    /// </summary>
    [JsonPropertyName("status")]
    public required SandboxStatus Status { get; set; }

    /// <summary>
    /// Gets or sets the effective platform used for sandbox provisioning.
    /// </summary>
    [JsonPropertyName("platform")]
    public PlatformSpec? Platform { get; set; }

    /// <summary>
    /// Gets or sets the custom metadata tags.
    /// </summary>
    [JsonPropertyName("metadata")]
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the sandbox expiration time.
    /// </summary>
    [JsonPropertyName("expiresAt")]
    public DateTime? ExpiresAt { get; set; }

    /// <summary>
    /// Gets or sets the sandbox creation time.
    /// </summary>
    [JsonPropertyName("createdAt")]
    public required DateTime CreatedAt { get; set; }

    /// <summary>
    /// Gets or sets the entrypoint command.
    /// </summary>
    [JsonPropertyName("entrypoint")]
    public required IReadOnlyList<string> Entrypoint { get; set; }
}

/// <summary>
/// Status of a snapshot.
/// </summary>
public class SnapshotStatus
{
    [JsonPropertyName("state")]
    public required string State { get; set; }

    [JsonPropertyName("reason")]
    public string? Reason { get; set; }

    [JsonPropertyName("message")]
    public string? Message { get; set; }

    [JsonPropertyName("lastTransitionAt")]
    public DateTime? LastTransitionAt { get; set; }
}

/// <summary>
/// Information about a snapshot.
/// </summary>
public class SnapshotInfo
{
    [JsonPropertyName("id")]
    public required string Id { get; set; }

    [JsonPropertyName("sandboxId")]
    public required string SandboxId { get; set; }

    [JsonPropertyName("name")]
    public string? Name { get; set; }

    [JsonPropertyName("status")]
    public required SnapshotStatus Status { get; set; }

    [JsonPropertyName("createdAt")]
    public required DateTime CreatedAt { get; set; }
}

/// <summary>
/// Request to create a snapshot.
/// </summary>
public class CreateSnapshotRequest
{
    [JsonPropertyName("name")]
    public string? Name { get; set; }
}

/// <summary>
/// Pagination information for list responses.
/// </summary>
public class PaginationInfo
{
    /// <summary>
    /// Gets or sets the current page number.
    /// </summary>
    [JsonPropertyName("page")]
    public int Page { get; set; }

    /// <summary>
    /// Gets or sets the page size.
    /// </summary>
    [JsonPropertyName("pageSize")]
    public int PageSize { get; set; }

    /// <summary>
    /// Gets or sets the total number of items.
    /// </summary>
    [JsonPropertyName("totalItems")]
    public int TotalItems { get; set; }

    /// <summary>
    /// Gets or sets the total number of pages.
    /// </summary>
    [JsonPropertyName("totalPages")]
    public int TotalPages { get; set; }

    /// <summary>
    /// Gets or sets whether there is a next page.
    /// </summary>
    [JsonPropertyName("hasNextPage")]
    public bool HasNextPage { get; set; }
}

/// <summary>
/// Response from listing sandboxes.
/// </summary>
public class ListSandboxesResponse
{
    /// <summary>
    /// Gets or sets the list of sandboxes.
    /// </summary>
    [JsonPropertyName("items")]
    public required IReadOnlyList<SandboxInfo> Items { get; set; }

    /// <summary>
    /// Gets or sets the pagination information.
    /// </summary>
    [JsonPropertyName("pagination")]
    public PaginationInfo? Pagination { get; set; }
}

/// <summary>
/// Parameters for listing sandboxes.
/// </summary>
public class ListSandboxesParams
{
    /// <summary>
    /// Gets or sets the states to filter by.
    /// </summary>
    public IReadOnlyList<string>? States { get; set; }

    /// <summary>
    /// Gets or sets the metadata to filter by.
    /// </summary>
    public IReadOnlyDictionary<string, string>? Metadata { get; set; }

    /// <summary>
    /// Gets or sets the page number.
    /// </summary>
    public int? Page { get; set; }

    /// <summary>
    /// Gets or sets the page size.
    /// </summary>
    public int? PageSize { get; set; }
}

/// <summary>
/// Response from listing snapshots.
/// </summary>
public class ListSnapshotsResponse
{
    [JsonPropertyName("items")]
    public required IReadOnlyList<SnapshotInfo> Items { get; set; }

    [JsonPropertyName("pagination")]
    public PaginationInfo? Pagination { get; set; }
}

/// <summary>
/// Parameters for listing snapshots.
/// </summary>
public class ListSnapshotsParams
{
    public string? SandboxId { get; set; }
    public IReadOnlyList<string>? States { get; set; }
    public int? Page { get; set; }
    public int? PageSize { get; set; }
}

/// <summary>
/// Request to renew sandbox expiration.
/// </summary>
public class RenewSandboxExpirationRequest
{
    /// <summary>
    /// Gets or sets the new expiration time as ISO 8601 string.
    /// </summary>
    [JsonPropertyName("expiresAt")]
    public required string ExpiresAt { get; set; }
}

/// <summary>
/// Response from renewing sandbox expiration.
/// </summary>
public class RenewSandboxExpirationResponse
{
    /// <summary>
    /// Gets or sets the updated expiration time.
    /// </summary>
    [JsonPropertyName("expiresAt")]
    public DateTime? ExpiresAt { get; set; }
}

/// <summary>
/// Endpoint information for a sandbox port.
/// </summary>
public class Endpoint
{
    /// <summary>
    /// Gets or sets the endpoint address (host:port or path).
    /// </summary>
    [JsonPropertyName("endpoint")]
    public required string EndpointAddress { get; set; }

    /// <summary>
    /// Gets or sets headers that must be included when calling this endpoint.
    /// </summary>
    [JsonPropertyName("headers")]
    public IReadOnlyDictionary<string, string> Headers { get; set; } = new Dictionary<string, string>();
}

/// <summary>
/// Known sandbox states.
/// </summary>
public static class SandboxStates
{
    /// <summary>
    /// Sandbox is being created.
    /// </summary>
    public const string Creating = "Creating";

    /// <summary>
    /// Sandbox is running.
    /// </summary>
    public const string Running = "Running";

    /// <summary>
    /// Sandbox is being paused.
    /// </summary>
    public const string Pausing = "Pausing";

    /// <summary>
    /// Sandbox is paused.
    /// </summary>
    public const string Paused = "Paused";

    /// <summary>
    /// Sandbox is being resumed.
    /// </summary>
    public const string Resuming = "Resuming";

    /// <summary>
    /// Sandbox is being deleted.
    /// </summary>
    public const string Deleting = "Deleting";

    /// <summary>
    /// Sandbox has been deleted.
    /// </summary>
    public const string Deleted = "Deleted";

    /// <summary>
    /// Sandbox is in an error state.
    /// </summary>
    public const string Error = "Error";
}
