// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package credentialvault

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// ResolveAdmission identifies the request's pinned decision snapshot on the
// private addon socket. It never supplies the resolver's wire sandbox identity.
type ResolveAdmission struct {
	SubjectKey string            `json:"subjectKey"`
	Revision   revision.Identity `json:"revision"`
}

// ReadResolveAdmission returns nil only for legacy pull-mode requests. New
// addons never mix an admission with a clientIp selector; malformed admission
// must never downgrade to that older lookup path.
func ReadResolveAdmission(r *http.Request) (*ResolveAdmission, error) {
	if r == nil {
		return nil, nil
	}
	query := r.URL.Query()
	values, present := query["admission"]
	if !present {
		return nil, nil
	}
	invalid := errors.New("invalid resolve admission")
	if len(values) != 1 || query.Has("clientIp") || len(values[0]) > 4096 {
		return nil, invalid
	}
	fields, err := strictJSONObject([]byte(values[0]))
	if err != nil || len(fields) != 2 {
		return nil, invalid
	}
	var admission ResolveAdmission
	if admission.SubjectKey, err = decodeStrictStringField(fields, "subjectKey"); err != nil {
		return nil, invalid
	}
	fields, err = strictJSONObject(fields["revision"])
	if err != nil || len(fields) != 6 {
		return nil, invalid
	}
	for _, key := range []string{"controlGeneration", "subjectGeneration", "decisionEpoch", "vaultRevision", "policyEpoch", "digest"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return nil, invalid
		}
	}
	if json.Unmarshal([]byte(values[0]), &admission) != nil {
		return nil, invalid
	}
	id := admission.Revision
	if id.ControlGeneration == "" || id.SubjectGeneration == "" || id.DecisionEpoch <= 0 || id.VaultRevision <= 0 || id.PolicyEpoch < 0 || len(id.Digest) != 64 {
		return nil, invalid
	}
	for _, c := range id.Digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, invalid
		}
	}
	return &admission, nil
}
