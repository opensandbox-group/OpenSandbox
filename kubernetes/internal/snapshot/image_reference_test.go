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

package snapshot

import "testing"

func TestImmutableImageReference(t *testing.T) {
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "tagged registry image",
			image: "registry.example.com/snapshots/main:snap-1",
			want:  "registry.example.com/snapshots/main@" + digest,
		},
		{
			name:  "registry port",
			image: "localhost:5000/snapshots/main:snap-1",
			want:  "localhost:5000/snapshots/main@" + digest,
		},
		{
			name:  "existing digest",
			image: "registry.example.com/snapshots/main@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want:  "registry.example.com/snapshots/main@" + digest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ImmutableImageReference(tt.image, digest)
			if err != nil {
				t.Fatalf("ImmutableImageReference returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImmutableImageReferenceRejectsInvalidDigest(t *testing.T) {
	if _, err := ImmutableImageReference("registry.example.com/main:tag", "sha256:short"); err == nil {
		t.Fatal("expected invalid digest error")
	}
}
