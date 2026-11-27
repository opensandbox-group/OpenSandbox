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

package iptables

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestForwardRedirectNftScript(t *testing.T) {
	s := forwardRedirectNftScript(18082)
	want := []string{
		"add chain ip opensandbox_forward_redirect output { type nat hook output priority -100; policy accept; }",
		"add rule ip opensandbox_forward_redirect output ip daddr 127.0.0.0/8 return",
		"add rule ip opensandbox_forward_redirect output meta mark 0x1 return",
		"add rule ip opensandbox_forward_redirect output meta mark 0x4 return",
		"add rule ip opensandbox_forward_redirect output meta mark 0x8 return",
		"add rule ip opensandbox_forward_redirect output ip daddr 169.254.0.0/16 return",
		"add rule ip opensandbox_forward_redirect output tcp dport 53 return",
		"add rule ip opensandbox_forward_redirect output meta l4proto tcp redirect to :18082",
	}
	last := -1
	for _, w := range want {
		idx := strings.Index(s, w)
		if idx < 0 {
			t.Fatalf("script missing %q:\n%s", w, s)
		}
		// Every exemption must precede the catch-all redirect.
		if idx < last {
			t.Fatalf("rule %q out of order:\n%s", w, s)
		}
		last = idx
	}
	// Phase 1 never relays IPv6; it is dropped by the filter table.
	if strings.Contains(s, "ip6") {
		t.Fatalf("forward redirect must not touch ip6:\n%s", s)
	}
}

func TestSetupForwardRedirectRetriesWithoutDeleteWhenTableIsMissing(t *testing.T) {
	var scripts []string
	r := redirectRunner{
		runNft: func(_ context.Context, script string) ([]byte, error) {
			scripts = append(scripts, script)
			if strings.Contains(script, "delete table ip opensandbox_forward_redirect") {
				return []byte("Error: Could not process rule: No such file or directory\ndelete table ip opensandbox_forward_redirect"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}
	if err := r.setupForwardRedirect(context.Background(), 18082); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(scripts) != 2 || strings.Contains(scripts[1], "delete table") {
		t.Fatalf("expected one retry without delete, got %q", scripts)
	}
}

func TestSetupForwardRedirectPropagatesFailure(t *testing.T) {
	r := redirectRunner{
		runNft: func(context.Context, string) ([]byte, error) {
			return []byte("Error: syntax error"), errors.New("exit status 1")
		},
	}
	if err := r.setupForwardRedirect(context.Background(), 18082); err == nil {
		t.Fatal("expected failure to propagate")
	}
}
