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

// Package opensandbox provides Go client libraries for the OpenSandbox
// Lifecycle, Egress, and Execd APIs.
package opensandbox

import (
	"encoding/json"
	"fmt"
	"time"
)

// SandboxState represents the high-level lifecycle state of a sandbox.
type SandboxState string

const (
	StatePending    SandboxState = "Pending"
	StateRunning    SandboxState = "Running"
	StatePausing    SandboxState = "Pausing"
	StatePaused     SandboxState = "Paused"
	StateStopping   SandboxState = "Stopping"
	StateTerminated SandboxState = "Terminated"
	StateFailed     SandboxState = "Failed"
)

// SandboxStatus provides detailed status information with lifecycle state
// and transition details.
type SandboxStatus struct {
	State            SandboxState `json:"state"`
	Reason           string       `json:"reason,omitempty"`
	Message          string       `json:"message,omitempty"`
	LastTransitionAt *time.Time   `json:"lastTransitionAt,omitempty"`
}

// ImageSpec describes the container image used to provision a sandbox.
type ImageSpec struct {
	URI  string     `json:"uri"`
	Auth *ImageAuth `json:"auth,omitempty"`
}

// ImageAuth holds registry authentication credentials for private images.
type ImageAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// PlatformOS is the target operating system of a sandbox platform constraint.
// The wire-level enum is enforced server-side; the constants below mirror the
// spec so Go callers can avoid stringly-typed typos.
type PlatformOS string

const (
	OSLinux   PlatformOS = "linux"
	OSWindows PlatformOS = "windows"
)

// PlatformArch is the target CPU architecture of a sandbox platform
// constraint.
type PlatformArch string

const (
	ArchAMD64 PlatformArch = "amd64"
	ArchARM64 PlatformArch = "arm64"
)

// PlatformSpec is a runtime platform constraint used for scheduling and
// provisioning. It is independent from Image and expresses the expected
// target OS and CPU architecture for sandbox execution.
//
// When omitted, the server applies its own default platform selection
// behavior. When provided, the runtime must satisfy the constraint or the
// request fails.
//
// See specs/sandbox-lifecycle.yml#/components/schemas/PlatformSpec.
type PlatformSpec struct {
	OS   PlatformOS   `json:"os"`
	Arch PlatformArch `json:"arch"`
}

// ResourceLimits defines runtime resource constraints as key-value pairs.
// Common keys: "cpu" (e.g. "500m"), "memory" (e.g. "512Mi"), "gpu" (e.g. "1").
type ResourceLimits map[string]string

// Volume defines a storage mount for a sandbox.
type Volume struct {
	Name      string `json:"name"`
	Host      *Host  `json:"host,omitempty"`
	PVC       *PVC   `json:"pvc,omitempty"`
	OSSFS     *OSSFS `json:"ossfs,omitempty"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	SubPath   string `json:"subPath,omitempty"`
}

// Host represents a host path bind mount backend.
type Host struct {
	Path string `json:"path"`
}

// PVC represents a platform-managed named volume backend.
type PVC struct {
	ClaimName                  string   `json:"claimName"`
	CreateIfNotExists          *bool    `json:"createIfNotExists,omitempty"`
	DeleteOnSandboxTermination *bool    `json:"deleteOnSandboxTermination,omitempty"`
	StorageClass               *string  `json:"storageClass,omitempty"`
	Storage                    *string  `json:"storage,omitempty"`
	AccessModes                []string `json:"accessModes,omitempty"`
}

// OSSFS represents an Alibaba Cloud OSS mount backend via ossfs.
type OSSFS struct {
	Bucket          string   `json:"bucket"`
	Endpoint        string   `json:"endpoint"`
	Version         string   `json:"version,omitempty"`
	Options         []string `json:"options,omitempty"`
	AccessKeyID     string   `json:"accessKeyId"`
	AccessKeySecret string   `json:"accessKeySecret"`
}

// NetworkPolicy defines the egress network policy for a sandbox.
type NetworkPolicy struct {
	DefaultAction string        `json:"defaultAction,omitempty"`
	Egress        []NetworkRule `json:"egress,omitempty"`
}

// NetworkRule defines a single egress allow/deny rule.
type NetworkRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// CredentialProxyConfig enables Credential Vault transparent proxy support at
// sandbox startup.
type CredentialProxyConfig struct {
	Enabled bool `json:"enabled"`
}

// CreateSandboxRequest is the request body for creating a new sandbox.
type CreateSandboxRequest struct {
	Image           *ImageSpec             `json:"image,omitempty"`
	SnapshotID      string                 `json:"snapshotId,omitempty"`
	Timeout         *int                   `json:"timeout,omitempty"`
	ResourceLimits  ResourceLimits         `json:"resourceLimits"`
	Env             map[string]string      `json:"env,omitempty"`
	SecureAccess    bool                   `json:"secureAccess,omitempty"`
	Metadata        map[string]string      `json:"metadata,omitempty"`
	Entrypoint      []string               `json:"entrypoint,omitempty"`
	NetworkPolicy   *NetworkPolicy         `json:"networkPolicy,omitempty"`
	CredentialProxy *CredentialProxyConfig `json:"credentialProxy,omitempty"`
	Volumes         []Volume               `json:"volumes,omitempty"`
	Extensions      map[string]string      `json:"extensions,omitempty"`
	Platform        *PlatformSpec          `json:"platform,omitempty"`
}

// SandboxInfo represents a runtime execution environment provisioned from a
// container image, as returned by the lifecycle API.
type SandboxInfo struct {
	ID         string            `json:"id"`
	Image      *ImageSpec        `json:"image,omitempty"`
	SnapshotID string            `json:"snapshotId,omitempty"`
	Status     SandboxStatus     `json:"status"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	Platform   *PlatformSpec     `json:"platform,omitempty"`
}

type SnapshotState string

const (
	SnapshotStateCreating SnapshotState = "Creating"
	SnapshotStateDeleting SnapshotState = "Deleting"
	SnapshotStateReady    SnapshotState = "Ready"
	SnapshotStateFailed   SnapshotState = "Failed"
)

type SnapshotStatus struct {
	State            SnapshotState `json:"state"`
	Reason           string        `json:"reason,omitempty"`
	Message          string        `json:"message,omitempty"`
	LastTransitionAt *time.Time    `json:"lastTransitionAt,omitempty"`
}

type SnapshotInfo struct {
	ID        string         `json:"id"`
	SandboxID string         `json:"sandboxId"`
	Name      string         `json:"name,omitempty"`
	Status    SnapshotStatus `json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
}

type CreateSnapshotRequest struct {
	Name string `json:"name,omitempty"`
}

// PaginationInfo contains pagination metadata for list responses.
type PaginationInfo struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	TotalItems  int  `json:"totalItems"`
	TotalPages  int  `json:"totalPages"`
	HasNextPage bool `json:"hasNextPage"`
}

// ListSandboxesResponse is the paginated response from listing sandboxes.
type ListSandboxesResponse struct {
	Items      []SandboxInfo  `json:"items"`
	Pagination PaginationInfo `json:"pagination"`
}

// MetadataPatch is the request body for patching sandbox metadata.
// Non-nil values add or replace keys. Nil values delete keys.
type MetadataPatch map[string]*string

type ListSnapshotsResponse struct {
	Items      []SnapshotInfo `json:"items"`
	Pagination PaginationInfo `json:"pagination"`
}

type ListSnapshotsOptions struct {
	SandboxID string
	States    []SnapshotState
	Page      int
	PageSize  int
}

// Endpoint describes a public access endpoint for a service running inside
// a sandbox.
type Endpoint struct {
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// RenewExpirationRequest is the request body for renewing sandbox expiration.
type RenewExpirationRequest struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// RenewExpirationResponse is the response from renewing sandbox expiration.
type RenewExpirationResponse struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// PolicyStatusResponse is the response from the egress policy endpoints.
type PolicyStatusResponse struct {
	Status          string         `json:"status,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	EnforcementMode string         `json:"enforcementMode,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	Policy          *NetworkPolicy `json:"policy,omitempty"`
}

// CredentialSourceType is the credential source discriminator.
type CredentialSourceType string

const (
	// CredentialSourceInline carries write-only inline credential material.
	CredentialSourceInline CredentialSourceType = "inline"
)

// InlineCredentialSource contains write-only credential material. Values sent
// in this model are never returned by Credential Vault state endpoints.
type InlineCredentialSource struct {
	Type  CredentialSourceType `json:"type"`
	Value string               `json:"value"`
}

// MarshalJSON defaults the only supported source type so callers can use the
// natural zero-value form InlineCredentialSource{Value: secret}.
func (s InlineCredentialSource) MarshalJSON() ([]byte, error) {
	type inlineCredentialSource InlineCredentialSource
	source := inlineCredentialSource(s)
	if source.Type == "" {
		source.Type = CredentialSourceInline
	}
	return json.Marshal(source)
}

// Credential is a sandbox-local Credential Vault credential create/update
// model.
type Credential struct {
	Name   string                 `json:"name"`
	Source InlineCredentialSource `json:"source"`
}

// CredentialScheme is a request scheme matched by a Credential Vault binding.
type CredentialScheme string

const (
	CredentialSchemeHTTPS CredentialScheme = "https"
	CredentialSchemeHTTP  CredentialScheme = "http"
)

// CredentialMatch selects outbound requests where a Credential Vault binding
// applies.
type CredentialMatch struct {
	Schemes []CredentialScheme `json:"schemes,omitempty"`
	Ports   []int              `json:"ports,omitempty"`
	Hosts   []string           `json:"hosts"`
	Methods []string           `json:"methods,omitempty"`
	Paths   []string           `json:"paths,omitempty"`
}

// CustomHeaderEntry describes one custom header injection rule.
type CustomHeaderEntry struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// CredentialAuthType is the Credential Vault auth discriminator.
type CredentialAuthType string

const (
	CredentialAuthBearer        CredentialAuthType = "bearer"
	CredentialAuthBasic         CredentialAuthType = "basic"
	CredentialAuthAPIKey        CredentialAuthType = "apiKey"
	CredentialAuthCustomHeaders CredentialAuthType = "customHeaders"
)

// CredentialAuth configures how a binding injects credential material into
// matching outbound requests.
type CredentialAuth struct {
	Type       CredentialAuthType  `json:"type"`
	Credential string              `json:"credential,omitempty"`
	Name       string              `json:"name,omitempty"`
	Headers    []CustomHeaderEntry `json:"headers,omitempty"`
}

// CredentialBinding is a sandbox-local Credential Vault binding create/update
// model.
type CredentialBinding struct {
	Name  string          `json:"name"`
	Match CredentialMatch `json:"match"`
	Auth  CredentialAuth  `json:"auth"`
}

// CredentialVaultCreateRequest creates the initial sandbox-local Credential
// Vault revision.
type CredentialVaultCreateRequest struct {
	Credentials []Credential        `json:"credentials"`
	Bindings    []CredentialBinding `json:"bindings"`
}

// CredentialMutationSet describes atomic credential changes for a vault patch.
type CredentialMutationSet struct {
	Add     []Credential `json:"add,omitempty"`
	Replace []Credential `json:"replace,omitempty"`
	Delete  []string     `json:"delete,omitempty"`
}

// CredentialBindingMutationSet describes atomic binding changes for a vault
// patch.
type CredentialBindingMutationSet struct {
	Add     []CredentialBinding `json:"add,omitempty"`
	Replace []CredentialBinding `json:"replace,omitempty"`
	Delete  []string            `json:"delete,omitempty"`
}

// CredentialVaultPatchRequest atomically mutates credentials and bindings. If
// ExpectedRevision is set, the sidecar applies it as an optimistic concurrency
// guard.
type CredentialVaultPatchRequest struct {
	ExpectedRevision *int                          `json:"expectedRevision,omitempty"`
	Credentials      *CredentialMutationSet        `json:"credentials,omitempty"`
	Bindings         *CredentialBindingMutationSet `json:"bindings,omitempty"`
}

// CredentialMetadata is sanitized credential metadata returned by Credential
// Vault. It intentionally does not include source values.
type CredentialMetadata struct {
	Name       string `json:"name"`
	SourceType string `json:"sourceType"`
	Revision   int    `json:"revision"`
}

// CredentialAuthMetadata is sanitized auth metadata returned by Credential
// Vault. It intentionally does not include credential references or values.
type CredentialAuthMetadata struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// CredentialBindingMetadata is sanitized binding metadata returned by
// Credential Vault.
type CredentialBindingMetadata struct {
	Name     string                  `json:"name"`
	Revision int                     `json:"revision"`
	Match    *CredentialMatch        `json:"match,omitempty"`
	Auth     *CredentialAuthMetadata `json:"auth,omitempty"`
}

// CredentialVaultState is sanitized Credential Vault state.
type CredentialVaultState struct {
	Revision    int                         `json:"revision"`
	Credentials []CredentialMetadata        `json:"credentials"`
	Bindings    []CredentialBindingMetadata `json:"bindings"`
}

// CredentialListResponse is the credential metadata list response.
type CredentialListResponse struct {
	Revision    int                  `json:"revision"`
	Credentials []CredentialMetadata `json:"credentials"`
}

// CredentialBindingListResponse is the binding metadata list response.
type CredentialBindingListResponse struct {
	Revision int                         `json:"revision"`
	Bindings []CredentialBindingMetadata `json:"bindings"`
}

// ErrorResponse is the standard error response for non-2xx HTTP responses.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError wraps an ErrorResponse with the HTTP status code and retry metadata.
type APIError struct {
	StatusCode int
	RequestID  string
	Response   ErrorResponse

	// RetryAfter is the server-suggested wait duration from the Retry-After
	// header. Zero means no suggestion was provided.
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Response.Code, e.Response.Message)
	if e.RequestID != "" {
		msg += fmt.Sprintf(" (request_id: %s)", e.RequestID)
	}
	return msg
}

// Execd types are hand-written: execd uses SSE streaming, multipart upload, and
// text responses that do not fit this SDK's higher-level API ergonomics.

// CodeContext represents a code execution context identifier and language.
type CodeContext struct {
	ID       string `json:"id,omitempty"`
	Language string `json:"language"`
}

// CreateContextRequest is the request body for creating a code execution context.
type CreateContextRequest struct {
	Language string `json:"language"`
}

// RunCodeRequest is the request body for executing code in a context.
type RunCodeRequest struct {
	Context *CodeContext `json:"context,omitempty"`
	Code    string       `json:"code"`
}

// Session represents a bash session with a unique identifier.
type Session struct {
	ID string `json:"session_id"`
}

// CreateSessionRequest is the optional request body for creating a bash session.
type CreateSessionRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

// RunCommandRequest is the request body for executing a shell command.
type RunCommandRequest struct {
	Command    string            `json:"command"`
	Cwd        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"`
	Timeout    int64             `json:"timeout,omitempty"`
	UID        *int32            `json:"uid,omitempty"`
	GID        *int32            `json:"gid,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
}

// RunInSessionRequest is the request body for running a command in an existing bash session.
type RunInSessionRequest struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int64  `json:"timeout,omitempty"`
}

// CommandStatusResponse contains the status of a command execution.
type CommandStatusResponse struct {
	ID         string     `json:"id"`
	Content    string     `json:"content"`
	Running    bool       `json:"running"`
	ExitCode   *int32     `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// CommandLogsResponse contains the stdout/stderr output and cursor for
// incremental log polling.
type CommandLogsResponse struct {
	Output string
	Cursor int64
}

// FileInfo contains file metadata including path and permissions.
type FileInfo struct {
	Path       string    `json:"path"`
	Type       string    `json:"type,omitempty"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	CreatedAt  time.Time `json:"created_at"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Mode       int       `json:"mode"`
}

// Permission defines file ownership and mode settings.
type Permission struct {
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode"`
}

// PermissionsRequest maps file paths to their desired permission settings.
type PermissionsRequest map[string]Permission

// MoveItem defines a single file move/rename operation.
type MoveItem struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

// MoveRequest is a list of file move/rename operations.
type MoveRequest []MoveItem

// ReplaceItem defines a text replacement operation for a single file.
type ReplaceItem struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ReplaceRequest maps file paths to their replacement operations.
type ReplaceRequest map[string]ReplaceItem

// ReplaceResult holds the result of a content replacement for a single file.
type ReplaceResult struct {
	ReplacedCount int `json:"replacedCount"`
}

// ReplaceResponse maps file paths to their replacement results.
type ReplaceResponse map[string]ReplaceResult

// FileMetadata is the metadata sent alongside file uploads.
type FileMetadata struct {
	Path  string `json:"path"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode,omitempty"`
}

// Metrics contains system resource usage metrics.
type Metrics struct {
	CPUCount   float64 `json:"cpu_count"`
	CPUUsedPct float64 `json:"cpu_used_pct"`
	MemTotalMB float64 `json:"mem_total_mib"`
	MemUsedMB  float64 `json:"mem_used_mib"`
	Timestamp  int64   `json:"timestamp"`
}
