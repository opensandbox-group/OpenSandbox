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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// fastSandboxSubjectTarget implements D2′. JSON encoding and domain separation
// are part of the contract; do not replace them with delimiter concatenation.
func fastSandboxSubjectTarget(uid string, fence subject.Fencing) (revision.SubjectTarget, error) {
	key := string(subject.FromSandboxUID(uid))
	if uid == "" || !utf8.ValidString(key) || utf8.RuneCountInString(key) > 256 ||
		!utf8.ValidString(fence.RuntimeInstanceID) || !utf8.ValidString(fence.AttachmentID) {
		return revision.SubjectTarget{}, revision.ErrInvalid
	}
	encoded, err := json.Marshal([2]string{fence.RuntimeInstanceID, fence.AttachmentID})
	if err != nil {
		return revision.SubjectTarget{}, revision.ErrInvalid
	}
	digest := sha256.Sum256(append([]byte("fsb-subject-generation/v1:"), encoded...))
	return revision.SubjectTarget{SubjectKey: key, SubjectGeneration: hex.EncodeToString(digest[:])}, nil
}
