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
	"time"
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

func TestPermission_UnmarshalJSON(t *testing.T) {
	for _, mode := range []string{"", `,"mode":null`, `,"mode":0`, `,"mode":644`, `,"mode":600,"mode":644`, `,"mode":null,"mode":600`, `,"mode":600,"mode":null`} {
		t.Run(mode, func(t *testing.T) {
			data := `{"owner":"alice","group":"staff"` + mode + `}`
			var perm Permission
			if err := json.Unmarshal([]byte(data), &perm); err != nil {
				t.Fatal(err)
			}
			var meta FileMetadata
			if err := json.Unmarshal([]byte(data), &meta); err != nil {
				t.Fatal(err)
			}
			if perm != meta.Permission {
				t.Errorf("permission = %+v, upload permission = %+v", perm, meta.Permission)
			}
			encoded, err := json.Marshal(perm)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 3 || perm.Owner != "alice" || perm.Group != "staff" {
				t.Errorf("wire fields or ownership changed: %s", encoded)
			}
		})
	}
}

func TestPermission_UnmarshalJSONRejectsStringMode(t *testing.T) {
	var perm Permission
	if err := json.Unmarshal([]byte(`{"mode":"644"}`), &perm); err == nil {
		t.Fatal("Unmarshal accepted a string mode")
	}
}

func TestFileInfo_JSONRoundTrip(t *testing.T) {
	want := FileInfo{
		Path: "/tmp/file", Type: "file", Size: 42,
		ModifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		CreatedAt:  time.Date(2026, 1, 1, 3, 4, 5, 0, time.UTC),
		Permission: Permission{Owner: "alice", Group: "staff", Mode: 640},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got FileInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestFileMetadata_UnmarshalJSONRejectsStringMode(t *testing.T) {
	var meta FileMetadata
	if err := json.Unmarshal([]byte(`{"path":"/tmp/file","mode":"644"}`), &meta); err == nil {
		t.Fatalf("Unmarshal accepted a string mode: %+v", meta)
	}
}
