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

// The per-subject vault file format (OSEP-0022 "Vault Storage"): a versioned
// server↔egress persistence contract, one file per subject at
// <egress-volume>/vault/<sandboxUid>.json. Conformance vectors live in
// testdata/vaultfile and pin the exact encoding both sides must agree on.
package credentialvault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// VaultFileVersion is the original write schema. This compatibility stage
// keeps writing v1 while safely reading v2 as read-only state.
const VaultFileVersion = 1

const VaultFileVersion2 = 2

type FileState string

const (
	FileStateActive  FileState = "active"
	FileStateDeleted FileState = "deleted"
	FileStateEmpty   FileState = "empty"
)

// MaxEncodedVaultFileBytes bounds one encoded vault file: a relay body up to
// maxCredentialVaultBodyBytes plus the envelope (version, fence, revision,
// JSON framing). Consumers sizing storage or transfers for vault files use
// this instead of re-deriving the body limit — a cap at the body size alone
// would reject files the egress itself wrote.
const MaxEncodedVaultFileBytes = maxCredentialVaultBodyBytes + 4<<10

// ErrUnsupportedFileVersion marks a DecodeVaultFile failure caused solely by
// an unknown schema version — structurally fine for a newer reader.
var ErrUnsupportedFileVersion = errors.New("unsupported vault file version")

// VaultFile is the versioned envelope of one subject's persisted vault
// revision. Incarnation + AttachmentID record the owning runtime fence from
// the SET_BINDING envelope; a registration under a different fence — a reset,
// or a rebind that changed only the attachment — finds the file stale.
type VaultFile struct {
	Version          int             `json:"v"`
	Incarnation      string          `json:"incarnation"`
	AttachmentID     string          `json:"attachmentId"`
	Revision         int64           `json:"revision"`
	Vault            json.RawMessage `json:"vault"`
	State            FileState       `json:"-"`
	StateLineage     string          `json:"-"`
	StateRevision    uint64          `json:"-"`
	InitializationID string          `json:"-"`
}

type vaultFileV2 struct {
	Version          int             `json:"v"`
	Incarnation      string          `json:"incarnation"`
	AttachmentID     string          `json:"attachmentId"`
	Revision         int64           `json:"revision"`
	State            FileState       `json:"state"`
	StateLineage     string          `json:"stateLineage"`
	StateRevision    uint64          `json:"stateRevision"`
	InitializationID string          `json:"initializationId,omitempty"`
	Vault            json.RawMessage `json:"vault"`
}

// VaultFileBody is the OSEP-0012 vault JSON carried in VaultFile.Vault —
// exactly the credential vault create shape.
type VaultFileBody struct {
	Credentials []Credential `json:"credentials"`
	Bindings    []Binding    `json:"bindings"`
}

// EncodeVaultFile renders the canonical bytes: version pinned to
// VaultFileVersion, the owning runtime fence, the revision, the vault body.
// The output is deterministic (sorted content comes from the callers) and
// ends with a trailing newline, matching the policy file convention.
func EncodeVaultFile(incarnation, attachmentID string, revision int64, body VaultFileBody) ([]byte, error) {
	incarnation = strings.TrimSpace(incarnation)
	if incarnation == "" {
		return nil, fmt.Errorf("vault file incarnation cannot be blank")
	}
	attachmentID = strings.TrimSpace(attachmentID)
	if attachmentID == "" {
		return nil, fmt.Errorf("vault file attachmentId cannot be blank")
	}
	if revision < 1 {
		return nil, fmt.Errorf("vault file revision %d must be positive", revision)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode vault file body: %w", err)
	}
	out, err := json.Marshal(VaultFile{
		Version:      VaultFileVersion,
		Incarnation:  incarnation,
		AttachmentID: attachmentID,
		Revision:     revision,
		Vault:        raw,
	})
	if err != nil {
		return nil, fmt.Errorf("encode vault file envelope: %w", err)
	}
	return append(out, '\n'), nil
}

// DecodeVaultFile parses v1 and the read-only v2 envelope. Unknown versions
// and v2 shapes this build cannot interpret are reported as unsupported so
// callers preserve the original bytes rather than quarantine or rewrite them.
func DecodeVaultFile(data []byte) (VaultFile, VaultFileBody, error) {
	var body VaultFileBody
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return VaultFile{}, body, fmt.Errorf("vault file is empty")
	}
	hintedVersion, hasHintedVersion, duplicateVersion := vaultFileVersionHint(trimmed)
	var disc struct {
		Version *int `json:"v"`
	}
	if err := json.Unmarshal(trimmed, &disc); err != nil {
		if hasHintedVersion && hintedVersion >= VaultFileVersion2 {
			return VaultFile{}, body, fmt.Errorf("%w %d (malformed v2+ envelope): %v", ErrUnsupportedFileVersion, hintedVersion, err)
		}
		return VaultFile{}, body, fmt.Errorf("decode vault file envelope: %w", err)
	}
	if duplicateVersion && hasHintedVersion && hintedVersion >= VaultFileVersion2 {
		return VaultFile{}, body, fmt.Errorf("%w %d (duplicate version fields are ambiguous)", ErrUnsupportedFileVersion, hintedVersion)
	}
	if disc.Version != nil && *disc.Version > VaultFileVersion2 {
		return VaultFile{}, body, fmt.Errorf("%w %d (newer than supported %d)", ErrUnsupportedFileVersion, *disc.Version, VaultFileVersion2)
	}
	if disc.Version != nil && *disc.Version == VaultFileVersion2 {
		vf, body, err := decodeVaultFileV2(trimmed)
		if err != nil {
			return VaultFile{}, VaultFileBody{}, fmt.Errorf("%w: v2 vault file cannot be interpreted safely: %v", ErrUnsupportedFileVersion, err)
		}
		return vf, body, nil
	}
	var vf VaultFile
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vf); err != nil {
		return VaultFile{}, body, fmt.Errorf("decode vault file envelope: %w", err)
	}
	if vf.Version != VaultFileVersion {
		return VaultFile{}, body, fmt.Errorf("invalid vault file version %d (want %d)", vf.Version, VaultFileVersion)
	}
	if strings.TrimSpace(vf.Incarnation) == "" {
		return VaultFile{}, body, fmt.Errorf("vault file incarnation cannot be blank")
	}
	if strings.TrimSpace(vf.AttachmentID) == "" {
		return VaultFile{}, body, fmt.Errorf("vault file attachmentId cannot be blank")
	}
	if vf.Revision < 1 {
		return VaultFile{}, body, fmt.Errorf("vault file revision %d must be positive", vf.Revision)
	}
	if len(vf.Vault) == 0 {
		return VaultFile{}, body, fmt.Errorf("vault file body is missing")
	}
	bodyDec := json.NewDecoder(bytes.NewReader(vf.Vault))
	bodyDec.DisallowUnknownFields()
	if err := bodyDec.Decode(&body); err != nil {
		return VaultFile{}, body, fmt.Errorf("decode vault file body: %w", err)
	}
	return vf, body, nil
}

// vaultFileVersionHint scans top-level version fields even when a later field
// is truncated. It records the highest numeric version and whether the field
// was repeated, so an ambiguous envelope containing v2 cannot be decoded as
// v1 just because the last duplicate happens to say v1.
func vaultFileVersionHint(data []byte) (int, bool, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return 0, false, false
	}
	version, found, duplicate := 0, false, false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return version, found, duplicate
		}
		key, ok := keyToken.(string)
		if !ok {
			return version, found, duplicate
		}
		if key == "v" {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return version, found, duplicate
			}
			var parsed *int
			if err := json.Unmarshal(raw, &parsed); err != nil || parsed == nil {
				continue
			}
			if found {
				duplicate = true
			}
			if *parsed > version {
				version = *parsed
			}
			found = true
			continue
		}
		var ignored json.RawMessage
		if err := dec.Decode(&ignored); err != nil {
			return version, found, duplicate
		}
	}
	return version, found, duplicate
}

func decodeVaultFileV2(data []byte) (VaultFile, VaultFileBody, error) {
	var wire vaultFileV2
	if err := decodeSingleJSON(data, &wire); err != nil {
		return VaultFile{}, VaultFileBody{}, err
	}
	if wire.Version != VaultFileVersion2 {
		return VaultFile{}, VaultFileBody{}, fmt.Errorf("invalid vault file version %d", wire.Version)
	}
	if strings.TrimSpace(wire.Incarnation) == "" || strings.TrimSpace(wire.AttachmentID) == "" {
		return VaultFile{}, VaultFileBody{}, errors.New("v2 vault file fence cannot be blank")
	}
	if strings.TrimSpace(wire.StateLineage) == "" || wire.StateRevision == 0 {
		return VaultFile{}, VaultFileBody{}, errors.New("v2 vault file order token is incomplete")
	}
	var body VaultFileBody
	if len(wire.Vault) == 0 || !json.Valid(wire.Vault) {
		return VaultFile{}, VaultFileBody{}, errors.New("v2 vault file body is missing or invalid")
	}
	if err := decodeSingleJSON(wire.Vault, &body); err != nil {
		return VaultFile{}, VaultFileBody{}, fmt.Errorf("decode v2 vault body: %w", err)
	}
	switch wire.State {
	case FileStateActive:
		if wire.Revision < 1 {
			return VaultFile{}, VaultFileBody{}, errors.New("active v2 vault revision must be positive")
		}
	case FileStateDeleted, FileStateEmpty:
		if wire.Revision != 0 || len(body.Credentials) != 0 || len(body.Bindings) != 0 {
			return VaultFile{}, VaultFileBody{}, fmt.Errorf("%s v2 vault must have revision 0 and an empty body", wire.State)
		}
		if wire.State == FileStateEmpty && strings.TrimSpace(wire.InitializationID) == "" {
			return VaultFile{}, VaultFileBody{}, errors.New("empty v2 vault requires an initialization ID")
		}
	default:
		return VaultFile{}, VaultFileBody{}, fmt.Errorf("unsupported v2 vault state %q", wire.State)
	}
	return VaultFile{
		Version: VaultFileVersion2, Incarnation: wire.Incarnation, AttachmentID: wire.AttachmentID,
		Revision: wire.Revision, Vault: append(json.RawMessage(nil), wire.Vault...), State: wire.State,
		StateLineage: wire.StateLineage, StateRevision: wire.StateRevision, InitializationID: wire.InitializationID,
	}, body, nil
}

func decodeSingleJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
