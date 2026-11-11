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

package model

import (
	"encoding/json"
	"testing"
)

func TestFileMetadata_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantMode    int
		wantModeSet bool
	}{
		{name: "omitted"},
		{name: "null", mode: `,"mode":null`},
		{name: "zero", mode: `,"mode":0`, wantModeSet: true},
		{name: "ordinary", mode: `,"mode":644`, wantMode: 644, wantModeSet: true},
		{name: "last numeric mode wins", mode: `,"mode":600,"mode":644`, wantMode: 644, wantModeSet: true},
		{name: "null then numeric mode", mode: `,"mode":null,"mode":600`, wantMode: 600, wantModeSet: true},
		{name: "numeric mode then null", mode: `,"mode":600,"mode":null`, wantMode: 600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := `{"path":"/tmp/file","owner":"alice","group":"staff","unknown":true` + tt.mode + `}`
			var meta FileMetadata
			if err := json.Unmarshal([]byte(data), &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Mode != tt.wantMode || meta.ModeSet != tt.wantModeSet {
				t.Errorf("Mode = %d, ModeSet = %t, want %d, %t", meta.Mode, meta.ModeSet, tt.wantMode, tt.wantModeSet)
			}
			if meta.Path != "/tmp/file" || meta.Owner != "alice" || meta.Group != "staff" {
				t.Errorf("path/owner/group did not decode: %+v", meta)
			}
		})
	}
}

func TestFileMetadata_UnmarshalJSONRejectsStringMode(t *testing.T) {
	var meta FileMetadata
	if err := json.Unmarshal([]byte(`{"path":"/tmp/file","mode":"644"}`), &meta); err == nil {
		t.Fatalf("Unmarshal accepted a string mode: %+v", meta)
	}
}
