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

// Package actionhandler models the fast-sandbox Sandbox Actions Handler
// protocol (docs/concepts/sandbox-actions.md, sandbox.fast.io/actions/v1)
// from the egress side: the egress process IS the Handler, and the Fastlet
// delivers Binding synchronization (SET_BINDING / REMOVE_BINDING), Lifecycle
// Hooks, and the pause/resume snapshot transfer (VAULT_EXPORT / VAULT_IMPORT
// for the credential vault, POLICY_EXPORT / POLICY_IMPORT for the recorded
// egress policy) over two Pod-loopback HTTP endpoints.
//
// The package owns the wire model, parsing, and validation only. Lifecycle
// semantics (deny-first registration, data-plane-ready activation) are wired
// by the fast-sandbox control plane (package main).
//
// Fail-closed rules:
//   - An unknown apiVersion, operation, Hook name, or malformed envelope is a
//     validation error; the caller must never act on a partially understood
//     message (the protocol's "never silently ignore" requirement).
//   - The fencing fields (revision.runtimeInstanceId + revision.attachmentId)
//     are required for every operation: an empty fence would make every
//     rebind look identical, so a reset could never be detected.
package actionhandler

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
)

// APIVersion is the only accepted protocol version.
const APIVersion = "sandbox.fast.io/actions/v1"
const APIVersionV2 = "sandbox.fast.io/actions/v2"

// Operation is the action operation code.
type Operation string

const (
	OperationSetBinding    Operation = "SET_BINDING"
	OperationLifecycleHook Operation = "LIFECYCLE_HOOK"
	OperationRemoveBinding Operation = "REMOVE_BINDING"
	// VaultExport/Import carry the subject's vault file across pause/resume
	// (OSEP-0022): export into the snapshot bundle before teardown, import
	// re-stamped before data-plane-ready — the only authorized incarnation
	// transfer.
	OperationVaultExport Operation = "VAULT_EXPORT"
	OperationVaultImport Operation = "VAULT_IMPORT"
	// PolicyExport/Import carry the recorded policy across pause/resume
	// with the same contract as the vault transfer.
	OperationPolicyExport Operation = "POLICY_EXPORT"
	OperationPolicyImport Operation = "POLICY_IMPORT"
)

// Lifecycle Hook names (v1). Unknown Hook names are rejected: the Handler
// must never silently ignore a subscribed checkpoint.
const (
	HookRuntimeReady   = "sandbox.runtime-ready"
	HookDataPlaneReady = "sandbox.data-plane-ready"
)

// SandboxRef identifies the sandbox the operation targets. UID is the
// subject identity; name/namespace are informational.
type SandboxRef struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Revision is the fencing and ordering material carried by every operation.
// RuntimeInstanceID and AttachmentID form the identity fence: a change in
// either means the sandbox was rebound and all prior state must be discarded.
// SpecGeneration is the desired-state version (bumps on ANY spec update,
// including policy updates, so it is deliberately NOT part of the identity
// fence); RouteGeneration is the data-plane route fence.
type Revision struct {
	SpecGeneration    uint64 `json:"specGeneration"`
	RuntimeInstanceID string `json:"runtimeInstanceId"`
	AttachmentID      string `json:"attachmentId"`
	RouteGeneration   uint64 `json:"routeGeneration"`
}

// Owner is the complete platform-validated runtime fence carried by Actions
// v2. It prevents delayed operations from being reinterpreted for a new slot
// occupant; it is not caller authentication.
type Owner struct {
	SandboxUID         string `json:"sandboxUid"`
	FastletPodUID      string `json:"fastletPodUid"`
	RuntimeInstanceID  string `json:"runtimeInstanceId"`
	InstanceGeneration int64  `json:"instanceGeneration"`
	AssignmentAttempt  int64  `json:"assignmentAttempt"`
	RouteGeneration    int64  `json:"routeGeneration"`
	AttachmentID       string `json:"attachmentId"`
}

func (o Owner) Valid() bool {
	return o.SandboxUID != "" && o.FastletPodUID != "" && o.RuntimeInstanceID != "" &&
		o.InstanceGeneration > 0 && o.AssignmentAttempt > 0 && o.RouteGeneration > 0 && o.AttachmentID != ""
}

func (o Owner) Matches(other Owner) bool { return o == other }

type OwnerObservation struct {
	State string `json:"state"`
	Owner Owner  `json:"owner"`
}

// NetworkAttachment is the network identity material the egress consumes:
// the dispatch key (SourceIP), the DNS/MITM targets (Gateway), the UDP
// spoofing defense (HostVeth iifname), and sibling isolation (PrivateCIDR).
type NetworkAttachment struct {
	IP          netip.Addr   `json:"ip"`
	Gateway     netip.Addr   `json:"gateway"`
	PrivateCIDR netip.Prefix `json:"privateCidr"`
	HostVeth    string       `json:"hostVeth"`
}

// Attachment wraps the network identity block of the envelope.
type Attachment struct {
	Network NetworkAttachment `json:"network"`
}

// Hook is a Lifecycle Hook notification. Sequence is the Hook delivery order
// for the Handler and generation.
type Hook struct {
	Name     string `json:"name"`
	Sequence uint64 `json:"sequence"`
}

// SnapshotTransfer names the driver-provided staging file for one subject's
// state snapshot (vault / recorded policy): the export target, or the bundle
// source at restore.
type SnapshotTransfer struct {
	Path string `json:"path"`
	// Seal selects the export semantics. Absent/nil = sealed (the
	// checkpoint flow: the subject is locked out of the copied state until
	// an import or teardown re-owns it). Seal=false = seal-free carry —
	// the record is copied verbatim and the source keeps full relay CRUD
	// (the template flow: the source stays live and the clone adopts the
	// carried state at its first registration).
	Seal *bool `json:"seal,omitempty"`
}

// Sealed reports the export semantics: nil means the checkpoint default
// (sealed).
func (t *SnapshotTransfer) Sealed() bool {
	return t.Seal == nil || *t.Seal
}

// validatePath enforces the Pod-local absolute-path contract at parse time,
// checking raw segments before normalization — the same contract the atomic
// file machinery enforces.
func (t *SnapshotTransfer) validatePath() error {
	path := strings.TrimSpace(t.Path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("snapshot path %q must be absolute", t.Path)
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("snapshot path %q must not traverse parents", t.Path)
		}
	}
	return nil
}

// Binding carries the Handler-owned input. Input is a JSON RawMessage so the
// three states are distinguishable: field absent (malformed), JSON null
// (binding removed from a still-live sandbox), and a JSON string (the opaque
// input value; "" and the literal string "null" are ordinary values).
type Binding struct {
	Input json.RawMessage `json:"input"`
}

// IsRemoval reports whether the binding input is the JSON literal null
// (binding removed from a still-live sandbox).
func (b *Binding) IsRemoval() bool {
	return len(b.Input) > 0 && string(b.Input) == "null"
}

// InputString returns the input as its string value. The field must have been
// validated first (Validate), so this never fails on a checked envelope.
func (b *Binding) InputString() (string, error) {
	if len(b.Input) == 0 {
		return "", fmt.Errorf("binding input absent")
	}
	var v string
	if err := json.Unmarshal(b.Input, &v); err != nil {
		return "", fmt.Errorf("binding input is not a string: %w", err)
	}
	return v, nil
}

// Envelope is the full request body of POST /_fastlet/v1/actions.
type Envelope struct {
	Mode         string            `json:"mode,omitempty"`
	APIVersion   string            `json:"apiVersion"`
	Operation    Operation         `json:"operation"`
	InvocationID string            `json:"invocationId"`
	Sandbox      SandboxRef        `json:"sandbox"`
	Revision     Revision          `json:"revision"`
	Owner        *Owner            `json:"owner,omitempty"`
	Attachment   *Attachment       `json:"attachment,omitempty"`
	Hook         *Hook             `json:"hook,omitempty"`
	Binding      *Binding          `json:"binding,omitempty"`
	Snapshot     *SnapshotTransfer `json:"snapshot,omitempty"`
}

// ParseEnvelope decodes and validates a request body. A returned error means
// the message is not a well-formed, understood action; the caller must fail
// closed (HTTP 400).
func ParseEnvelope(body []byte) (*Envelope, error) {
	return ParseEnvelopeVersion(body, APIVersion)
}

func ParseEnvelopeV2(body []byte) (*Envelope, error) {
	return ParseEnvelopeVersion(body, APIVersionV2)
}

func ParseEnvelopeVersion(body []byte, version string) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("invalid action envelope: %w", err)
	}
	if err := e.ValidateVersion(version); err != nil {
		return nil, err
	}
	return &e, nil
}

// Validate checks the protocol-level invariants. Field-level semantics (fence
// matching, registration state) are the caller's responsibility.
func (e *Envelope) Validate() error {
	return e.ValidateVersion(APIVersion)
}

func (e *Envelope) ValidateVersion(version string) error {
	if e.APIVersion != version {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", e.APIVersion, version)
	}
	if e.Sandbox.UID == "" {
		return fmt.Errorf("sandbox.uid is required")
	}
	if version == APIVersionV2 {
		if e.Mode != "legacy" {
			return fmt.Errorf("Actions v2 requires explicit legacy mode; other modes are unsupported")
		}
		if e.Owner == nil || !e.Owner.Valid() || e.Owner.SandboxUID != e.Sandbox.UID ||
			e.Owner.RuntimeInstanceID != e.Revision.RuntimeInstanceID || e.Owner.AttachmentID != e.Revision.AttachmentID ||
			uint64(e.Owner.RouteGeneration) != e.Revision.RouteGeneration {
			return fmt.Errorf("Actions v2 requires an owner matching the sandbox and revision")
		}
	} else if e.Owner != nil {
		return fmt.Errorf("Actions v1 must not carry an Actions v2 owner")
	}
	switch e.Operation {
	case OperationSetBinding:
		if e.Binding == nil {
			return fmt.Errorf("SET_BINDING requires a binding payload")
		}
		if !e.Binding.IsRemoval() {
			if _, err := e.Binding.InputString(); err != nil {
				return err
			}
			if err := e.requireAttachment(); err != nil {
				return err
			}
		}
	case OperationLifecycleHook:
		if e.Hook == nil || e.Hook.Name == "" {
			return fmt.Errorf("LIFECYCLE_HOOK requires a hook payload")
		}
		switch e.Hook.Name {
		case HookRuntimeReady, HookDataPlaneReady:
		default:
			return fmt.Errorf("unknown lifecycle hook %q", e.Hook.Name)
		}
	case OperationRemoveBinding:
		// no payload required; terminal cleanup is identified by the sandbox ref
	case OperationVaultExport, OperationVaultImport,
		OperationPolicyExport, OperationPolicyImport:
		if e.Snapshot == nil || strings.TrimSpace(e.Snapshot.Path) == "" {
			return fmt.Errorf("%s requires a snapshot payload with a path", e.Operation)
		}
		if err := e.Snapshot.validatePath(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown operation %q", e.Operation)
	}
	if e.Revision.RuntimeInstanceID == "" || e.Revision.AttachmentID == "" {
		return fmt.Errorf("revision.runtimeInstanceId and revision.attachmentId are required")
	}
	return nil
}

// requireAttachment checks the attachment network block on operations that
// carry the sandbox's network identity.
func (e *Envelope) requireAttachment() error {
	if e.Attachment == nil || !e.Attachment.Network.IP.IsValid() {
		return fmt.Errorf("attachment.network.ip is required")
	}
	if e.Attachment.Network.HostVeth == "" {
		return fmt.Errorf("attachment.network.hostVeth is required")
	}
	return nil
}

// Network returns the attachment's network block; empty when absent.
func (e *Envelope) Network() NetworkAttachment {
	if e.Attachment == nil {
		return NetworkAttachment{}
	}
	return e.Attachment.Network
}

// Fencing returns the identity fence for this envelope.
func (e *Envelope) Fencing() (runtimeInstanceID, attachmentID string) {
	return e.Revision.RuntimeInstanceID, e.Revision.AttachmentID
}

// StatusResponse is the body of GET /_fastlet/v1/actions/status. instanceId
// identifies one Handler process incarnation: a changed value makes the
// Fastlet invalidate Binding readiness and replay the latest SetBinding
// followed by the already-reached Hooks.
//
// The MitmCa* fields are additive observability (OSEP-0022 pinned CA):
// consumers unaware of them ignore the extra JSON keys.
type StatusResponse struct {
	APIVersion  string `json:"apiVersion"`
	Ready       bool   `json:"ready"`
	InstanceID  string `json:"instanceId"`
	OwnerStatus bool   `json:"ownerStatus,omitempty"`
	// MitmCaFingerprint is the pinned MITM CA's SHA-256 fingerprint
	// (lowercase hex over the DER); empty when MITM is disabled or no CA is
	// provisioned.
	MitmCaFingerprint string `json:"mitmCaFingerprint,omitempty"`
	// MitmCaDown is the explicit MITM-addon-down signal: the pinned CA is
	// missing or unloadable, the addon refused to start, and the state is
	// latched until process restart. Policy actions keep serving.
	MitmCaDown bool `json:"mitmCaDown,omitempty"`
	// MitmCaError carries the addon-down reason (empty when up).
	MitmCaError string `json:"mitmCaError,omitempty"`

	// Vault persistence health (OSEP-0022 stage 2, additive): Recovery is
	// true while the recovery latch freezes vault writes until restart;
	// DegradedVaults lists subjects whose persisted vault failed to load
	// (interception fails closed for them; a successful write clears it).
	Recovery       bool     `json:"vaultRecovery,omitempty"`
	DegradedVaults []string `json:"degradedVaults,omitempty"`
	// VaultTransfer reports that this Handler serves VAULT_EXPORT /
	// VAULT_IMPORT and persists vault files. Absent on older builds decodes
	// as false: the Fastlet then skips the transfer (checkpoint proceeds
	// without a vault) instead of failing it.
	VaultTransfer bool `json:"vaultTransfer,omitempty"`
	// PolicyTransfer reports that this Handler serves POLICY_EXPORT /
	// POLICY_IMPORT and persists per-subject policy files.
	PolicyTransfer bool `json:"policyTransfer,omitempty"`
	// PolicyRecovery is true while the policy persistence recovery latch
	// freezes policy relay writes until restart; DegradedPolicies lists
	// subjects whose persisted policy failed to load (default-deny fail
	// closed for them; a relay write heals).
	PolicyRecovery   bool     `json:"policyRecovery,omitempty"`
	DegradedPolicies []string `json:"degradedPolicies,omitempty"`
	// TemplateStateExport reports that this Handler supports the
	// seal-free template state carry (SnapshotTransfer.Seal=false exports
	// keep the source live; imports land for the next registration).
	// Absent on older builds decodes as false: the Fastlet's template
	// snapshots then export nothing (handlers that always seal are never
	// asked) — rolling upgrades are safe in both directions.
	TemplateStateExport bool `json:"templateStateExport,omitempty"`
}
