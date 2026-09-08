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

// Package actionhandler models the fast-sandbox Sandbox Actions Handler
// protocol (docs/concepts/sandbox-actions.md, sandbox.fast.io/actions/v1)
// from the egress side: the egress process IS the Handler, and the Fastlet
// delivers Binding synchronization (SET_BINDING / REMOVE_BINDING) and
// Lifecycle Hooks over two Pod-loopback HTTP endpoints.
//
// The package owns the wire model, parsing, and validation only. Lifecycle
// semantics (deny-first registration, data-plane-ready activation) are wired
// by the fleet control plane (package main).
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
)

// APIVersion is the only accepted protocol version.
const APIVersion = "sandbox.fast.io/actions/v1"

// Operation is the action operation code.
type Operation string

const (
	OperationSetBinding    Operation = "SET_BINDING"
	OperationLifecycleHook Operation = "LIFECYCLE_HOOK"
	OperationRemoveBinding Operation = "REMOVE_BINDING"
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
	APIVersion   string      `json:"apiVersion"`
	Operation    Operation   `json:"operation"`
	InvocationID string      `json:"invocationId"`
	Sandbox      SandboxRef  `json:"sandbox"`
	Revision     Revision    `json:"revision"`
	Attachment   *Attachment `json:"attachment,omitempty"`
	Hook         *Hook       `json:"hook,omitempty"`
	Binding      *Binding    `json:"binding,omitempty"`
}

// ParseEnvelope decodes and validates a request body. A returned error means
// the message is not a well-formed, understood action; the caller must fail
// closed (HTTP 400).
func ParseEnvelope(body []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("invalid action envelope: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// Validate checks the protocol-level invariants. Field-level semantics (fence
// matching, registration state) are the caller's responsibility.
func (e *Envelope) Validate() error {
	if e.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", e.APIVersion, APIVersion)
	}
	if e.Sandbox.UID == "" {
		return fmt.Errorf("sandbox.uid is required")
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
type StatusResponse struct {
	APIVersion string `json:"apiVersion"`
	Ready      bool   `json:"ready"`
	InstanceID string `json:"instanceId"`
}
