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

package main

import "testing"

func TestParseFSGroup(t *testing.T) {
	tests := []struct {
		raw  string
		want *int
	}{
		{raw: ""},
		{raw: "1", want: intPointer(1)},
		{raw: "2147483647", want: intPointer(2147483647)},
	}
	for _, test := range tests {
		got, err := parseFSGroup(test.raw)
		if err != nil {
			t.Errorf("parseFSGroup(%q): %v", test.raw, err)
			continue
		}
		if (got == nil) != (test.want == nil) || got != nil && *got != *test.want {
			t.Errorf("parseFSGroup(%q) = %v, want %v", test.raw, got, test.want)
		}
	}

	for _, raw := range []string{"0", "-1", "2147483648", "invalid"} {
		if _, err := parseFSGroup(raw); err == nil {
			t.Errorf("parseFSGroup(%q) succeeded", raw)
		}
	}
}

func intPointer(value int) *int {
	return &value
}
