// Copyright 2025 Alibaba Group Holding Ltd.
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

package utils

import "testing"

func TestHeadTailBuffer(t *testing.T) {
	buffer := NewHeadTailBuffer(4, 4, "<truncated>")
	for _, chunk := range [][]byte{[]byte("abcdef"), []byte("ghijklmnop")} {
		written, err := buffer.Write(chunk)
		if err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if written != len(chunk) {
			t.Fatalf("Write() = %d, want %d", written, len(chunk))
		}
	}

	if got, want := buffer.String(), "abcd<truncated>mnop"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestTruncateHeadTail(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "within limit", value: "abcdefgh", want: "abcdefgh"},
		{name: "over limit", value: "abcdefghijkl", want: "abcd<truncated>ijkl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncateHeadTail(tt.value, 4, 4, "<truncated>"); got != tt.want {
				t.Fatalf("TruncateHeadTail() = %q, want %q", got, tt.want)
			}
		})
	}
}
