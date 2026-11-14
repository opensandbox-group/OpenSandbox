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

// The per-subject egress policy file format: a versioned persistence
// contract, one file per subject at <egress-volume>/policy/<sandboxUid>.json.
// The shape mirrors the credential vault file (pkg/credentialvault): a
// versioned envelope stamped with the owning runtime fence, the revision,
// and the recorded policy body.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PolicyFileVersion is the only supported on-disk schema version. Anything
// else — including a missing one — is rejected rather than misinterpreted;
// the rejection is identifiable via ErrUnsupportedPolicyFileVersion so
// callers can preserve files written by a newer consumer.
const PolicyFileVersion = 1

// MaxPolicyBodyBytes bounds one recorded policy body: the fast-sandbox
// Sandbox CRD caps every action binding input at 64 KiB, and the relay
// surface carries exactly that policy.
const MaxPolicyBodyBytes = 64 << 10

// MaxEncodedPolicyFileBytes bounds one encoded policy file: a relay body up
// to MaxPolicyBodyBytes plus the envelope (version, fence, revision, JSON
// framing). Consumers sizing storage or transfers for policy files use this
// instead of re-deriving the body limit.
const MaxEncodedPolicyFileBytes = MaxPolicyBodyBytes + 4<<10

// ErrUnsupportedPolicyFileVersion marks a DecodePolicyFile failure caused
// solely by an unknown schema version — structurally fine for a newer
// reader.
var ErrUnsupportedPolicyFileVersion = errors.New("unsupported policy file version")

// PolicyFile is the versioned envelope of one subject's persisted policy
// revision. Incarnation + AttachmentID record the owning runtime fence from
// the SET_BINDING envelope; a registration under a different fence — a
// reset, or a rebind that changed only the attachment — finds the file
// stale.
type PolicyFile struct {
	Version      int             `json:"v"`
	Incarnation  string          `json:"incarnation"`
	AttachmentID string          `json:"attachmentId"`
	Revision     int64           `json:"revision"`
	Policy       json.RawMessage `json:"policy"`
}

// EncodePolicyFile renders the canonical bytes: version pinned to
// PolicyFileVersion, the owning runtime fence, the revision, and the policy
// body. The body must already be canonical JSON (the encoded NetworkPolicy)
// and is stored verbatim so a decoded file re-parses to the same policy.
// The output ends with a trailing newline, matching the policy file
// convention.
func EncodePolicyFile(incarnation, attachmentID string, revision int64, body []byte) ([]byte, error) {
	incarnation = strings.TrimSpace(incarnation)
	if incarnation == "" {
		return nil, fmt.Errorf("policy file incarnation cannot be blank")
	}
	attachmentID = strings.TrimSpace(attachmentID)
	if attachmentID == "" {
		return nil, fmt.Errorf("policy file attachmentId cannot be blank")
	}
	if revision < 1 {
		return nil, fmt.Errorf("policy file revision %d must be positive", revision)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("policy file body is not valid JSON")
	}
	out, err := json.Marshal(PolicyFile{
		Version:      PolicyFileVersion,
		Incarnation:  incarnation,
		AttachmentID: attachmentID,
		Revision:     revision,
		Policy:       bytes.TrimSpace(body),
	})
	if err != nil {
		return nil, fmt.Errorf("encode policy file envelope: %w", err)
	}
	if len(out)+1 > MaxEncodedPolicyFileBytes {
		// Enforce the cap at encode time: an oversized fence field or body
		// must not produce a file our own importer would reject.
		return nil, fmt.Errorf("encoded policy file exceeds %d bytes", MaxEncodedPolicyFileBytes)
	}
	return append(out, '\n'), nil
}

// DecodePolicyFile parses and validates file bytes, returning the envelope
// and the raw policy body. The version discriminator is read FIRST with a
// tolerant view: a newer envelope carries unknown fields, so the strict v1
// schema must not reject (and the caller quarantine) a file this reader
// should merely preserve. Only strictly newer versions surface
// ErrUnsupportedPolicyFileVersion; a missing or invalid version is plain
// corruption. Semantic policy validation (ParsePolicy) is the caller's job —
// the file contract only pins the envelope.
func DecodePolicyFile(data []byte) (PolicyFile, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return PolicyFile{}, nil, fmt.Errorf("policy file is empty")
	}
	var disc struct {
		Version *int `json:"v"`
	}
	if err := json.Unmarshal(trimmed, &disc); err != nil {
		return PolicyFile{}, nil, fmt.Errorf("decode policy file envelope: %w", err)
	}
	if disc.Version != nil && *disc.Version > PolicyFileVersion {
		return PolicyFile{}, nil, fmt.Errorf("%w %d (newer than supported %d)", ErrUnsupportedPolicyFileVersion, *disc.Version, PolicyFileVersion)
	}
	var pf PolicyFile
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil {
		return PolicyFile{}, nil, fmt.Errorf("decode policy file envelope: %w", err)
	}
	if pf.Version != PolicyFileVersion {
		return PolicyFile{}, nil, fmt.Errorf("invalid policy file version %d (want %d)", pf.Version, PolicyFileVersion)
	}
	if strings.TrimSpace(pf.Incarnation) == "" {
		return PolicyFile{}, nil, fmt.Errorf("policy file incarnation cannot be blank")
	}
	if strings.TrimSpace(pf.AttachmentID) == "" {
		return PolicyFile{}, nil, fmt.Errorf("policy file attachmentId cannot be blank")
	}
	if pf.Revision < 1 {
		return PolicyFile{}, nil, fmt.Errorf("policy file revision %d must be positive", pf.Revision)
	}
	if len(pf.Policy) == 0 {
		return PolicyFile{}, nil, fmt.Errorf("policy file body is missing")
	}
	return pf, pf.Policy, nil
}
