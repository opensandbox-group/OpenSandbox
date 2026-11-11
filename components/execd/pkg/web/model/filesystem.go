// Copyright 2025 The OpenSandbox Authors
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

package model

import (
	"encoding/json"
	"time"
)

type FileInfo struct {
	Path       string    `json:"path,omitempty"`
	Type       string    `json:"type,omitempty"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	Permission `json:",inline"`
}

type FileMetadata struct {
	Path       string `json:"path,omitempty"`
	Permission `json:",inline"`

	// ModeSet reports whether the last mode field was non-null, so an
	// explicit 0 can be told apart from omission. It is never encoded.
	ModeSet bool `json:"-"`
}

// UnmarshalJSON records mode presence along with the decoded permissions.
func (m *FileMetadata) UnmarshalJSON(data []byte) error {
	type plain FileMetadata
	aux := struct {
		*plain
		Mode fileMetadataMode `json:"mode"`
	}{
		plain: (*plain)(m),
		Mode:  fileMetadataMode{value: m.Mode},
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.Mode = aux.Mode.value
	m.ModeSet = aux.Mode.set
	return nil
}

// fileMetadataMode preserves int decoding semantics across duplicate keys:
// null clears presence without discarding an earlier numeric value.
type fileMetadataMode struct {
	value int
	set   bool
}

func (m *fileMetadataMode) UnmarshalJSON(data []byte) error {
	var mode *int
	if err := json.Unmarshal(data, &mode); err != nil {
		return err
	}
	m.set = mode != nil
	if mode != nil {
		m.value = *mode
	}
	return nil
}

type Permission struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  int    `json:"mode"`
}

type RenameFileItem struct {
	Src  string `json:"src,omitempty"`
	Dest string `json:"dest,omitempty"`
}

type ReplaceFileContentItem struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

type ReplaceFileContentResult struct {
	ReplacedCount int `json:"replacedCount"`
}
