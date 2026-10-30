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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// Owner records outlive policy/Vault deletion. A retired UID is not reusable
// in this first strict slice; callers create a new UID instead of rebinding.
type strictOwnerRecord struct {
	Version       string               `json:"version"`
	Mode          string               `json:"mode"`
	Owner         actionhandler.Owner  `json:"owner"`
	Phase         string               `json:"phase"`
	VaultContract *strictVaultContract `json:"vaultContract,omitempty"`
}

// Disabling the opt-in must not turn an existing strict domain into a v1
// writer. Older binaries remain outside this deployment contract.
func validateOwnerDomain(dir, version string) error {
	if version == constants.ActionsAPIVersionV2 || strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json.owner") {
			return fmt.Errorf("strict owner records exist; Actions v1 cannot open this domain")
		}
	}
	return nil
}

func (s *fastSandboxPolicyServer) ownerFile(subj subject.Subject) (vaultFileStore, error) {
	s.mu.Lock()
	p := s.policyFiles
	s.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("strict owner persistence is unavailable")
	}
	// Use the same trusted directory and atomic writer, but never the policy
	// filename: ordinary policy deletion must not erase the owner fence.
	p.mu.Lock()
	defer p.mu.Unlock()
	path, err := p.pathFor(subj)
	if err != nil {
		return nil, err
	}
	return p.newFile(filepath.Clean(path) + ".owner")
}

// The caller holds the subject owner stripe across this read and its effects.
func (s *fastSandboxPolicyServer) readOwnerRecord(subj subject.Subject) (*strictOwnerRecord, error) {
	f, err := s.ownerFile(subj)
	if err != nil {
		return nil, err
	}
	snapshot, err := f.Snapshot()
	if err != nil || !snapshot.Exists() {
		return nil, err
	}
	var record strictOwnerRecord
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Data()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("invalid strict owner record: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || record.Version != constants.ActionsAPIVersionV2 ||
		record.Mode != "legacy" || !record.Owner.Valid() || subject.FromSandboxUID(record.Owner.SandboxUID) != subj ||
		(record.Phase != "active" && record.Phase != "retiring" && record.Phase != "retired") {
		return nil, fmt.Errorf("unsupported or inconsistent strict owner record")
	}
	if record.VaultContract != nil && !record.VaultContract.valid() {
		return nil, fmt.Errorf("unsupported Vault writer contract")
	}
	return &record, nil
}

func (s *fastSandboxPolicyServer) writeOwnerRecord(subj subject.Subject, owner actionhandler.Owner, phase string) error {
	f, err := s.ownerFile(subj)
	if err != nil {
		return err
	}
	record, err := s.readOwnerRecord(subj)
	if err != nil {
		return err
	}
	if record == nil {
		record = &strictOwnerRecord{Version: constants.ActionsAPIVersionV2, Mode: "legacy", Owner: owner}
	}
	if !record.Owner.Matches(owner) {
		return fmt.Errorf("owner changed")
	}
	record.Phase = phase
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	state, err := f.SaveRaw(data)
	if err != nil || state != policy.FileCommitted {
		return fmt.Errorf("strict owner persistence not confirmed (state %v): %v", state, err)
	}
	return nil
}

func (s *fastSandboxPolicyServer) cleanOwnerSubject(subj subject.Subject) error {
	s.mu.Lock()
	residual := s.subjectHasLiveStateLocked(subj)
	s.mu.Unlock()
	_, registered := s.reg.Fence(subj)
	if residual || registered {
		return fmt.Errorf("unclassified subject runtime state exists")
	}
	residual, err := s.subjectHasPersistentState(subj)
	if err != nil || residual {
		return fmt.Errorf("subject persistence is not confirmed empty")
	}
	return nil
}
