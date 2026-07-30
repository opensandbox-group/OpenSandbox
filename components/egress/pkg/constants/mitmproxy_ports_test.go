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

package constants

import (
	"strings"
	"testing"
)

func TestBuildMitmproxyPortList_ValidCases(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "80,443"},
		{"   ", "80,443"},
		{"8080", "80,443,8080"},
		{"8080,8443", "80,443,8080,8443"},
		{" 8080 , 8443 ", "80,443,8080,8443"},
	}
	for _, tc := range cases {
		got, err := BuildMitmproxyPortList(tc.raw)
		if err != nil {
			t.Errorf("BuildMitmproxyPortList(%q) unexpected err: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BuildMitmproxyPortList(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestBuildMitmproxyPortList_Invalid(t *testing.T) {
	cases := []struct {
		raw     string
		wantSub string
	}{
		{"abc", "not an integer"},
		{"0", "out of range"},
		{"65536", "out of range"},
		{"-1", "out of range"},
		{"80", "duplicate or reserved"},
		{"443", "duplicate or reserved"},
		{"8080,8080", "duplicate or reserved"},
		{"8080,,9000", "empty entry"},
		// 14 extras + 80 + 443 = 16 > 15 cap
		{"1,2,3,4,5,6,7,8,9,10,11,12,13,14", "exceeds iptables multiport limit"},
	}
	for _, tc := range cases {
		_, err := BuildMitmproxyPortList(tc.raw)
		if err == nil {
			t.Errorf("BuildMitmproxyPortList(%q) expected error, got nil", tc.raw)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("BuildMitmproxyPortList(%q) err = %v, want substring %q", tc.raw, err, tc.wantSub)
		}
	}
}

func TestBuildMitmproxyPortList_MaxAllowed(t *testing.T) {
	// 13 extras + 80 + 443 = 15, right at the cap.
	raw := "1,2,3,4,5,6,7,8,9,10,11,12,13"
	got, err := BuildMitmproxyPortList(raw)
	if err != nil {
		t.Fatalf("unexpected err at cap: %v", err)
	}
	if !strings.HasPrefix(got, "80,443,1,") {
		t.Errorf("unexpected result at cap: %q", got)
	}
}
