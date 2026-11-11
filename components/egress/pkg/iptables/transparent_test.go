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

func TestTransparentNftScriptCoversBothFamilies(t *testing.T) {
	s := transparentNftScript(18081, 10042, "80,443,8443")
	for _, want := range []string{
		"delete table ip opensandbox_mitm_redirect",
		"delete table ip6 opensandbox_mitm_redirect",
		"add chain ip opensandbox_mitm_redirect output { type nat hook output priority -100; policy accept; }",
		"add chain ip6 opensandbox_mitm_redirect output { type nat hook output priority -100; policy accept; }",
		"add rule ip opensandbox_mitm_redirect output ip daddr 127.0.0.0/8 tcp dport { 80, 443, 8443 } return",
		"add rule ip6 opensandbox_mitm_redirect output ip6 daddr ::1 tcp dport { 80, 443, 8443 } return",
		"add rule ip opensandbox_mitm_redirect output meta skuid 10042 tcp dport { 80, 443, 8443 } return",
		"add rule ip6 opensandbox_mitm_redirect output meta skuid 10042 tcp dport { 80, 443, 8443 } return",
		"add rule ip opensandbox_mitm_redirect output tcp dport { 80, 443, 8443 } redirect to :18081",
		"add rule ip6 opensandbox_mitm_redirect output tcp dport { 80, 443, 8443 } redirect to :18081",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q:\n%s", want, s)
		}
	}
	// The skip-uid rule must come BEFORE the redirect in each family, or mitmproxy loops on itself.
	for _, fam := range []string{"ip ", "ip6 "} {
		skip := strings.Index(s, "add rule "+fam+"opensandbox_mitm_redirect output meta skuid")
		redir := strings.Index(s, "add rule "+fam+"opensandbox_mitm_redirect output tcp dport")
		if skip < 0 || redir < 0 || skip > redir {
			t.Fatalf("%sskip-uid rule must precede the redirect", fam)
		}
	}
}

func TestSetupTransparentNftRetriesWithoutDeleteWhenTableIsMissing(t *testing.T) {
	var scripts []string
	r := redirectRunner{
		runNft: func(_ context.Context, script string) ([]byte, error) {
			scripts = append(scripts, script)
			if strings.Contains(script, "delete table ip opensandbox_mitm_redirect") {
				return []byte("Error: Could not process rule: No such file or directory\ndelete table ip opensandbox_mitm_redirect"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}
	if err := setupTransparentNft(context.Background(), r, 18081, 10042, "80,443"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(scripts) != 2 {
		t.Fatalf("expected a retry without the delete lines, got %d scripts", len(scripts))
	}
	if strings.Contains(scripts[1], "delete table") {
		t.Fatalf("retry still deletes:\n%s", scripts[1])
	}
}

func TestRedirectBackendFromEnv(t *testing.T) {
	t.Setenv(RedirectBackendEnv, "")
	if redirectBackend() != backendAuto {
		t.Fatal("empty → auto")
	}
	t.Setenv(RedirectBackendEnv, "NFT")
	if redirectBackend() != backendNft {
		t.Fatal("NFT → nft")
	}
	t.Setenv(RedirectBackendEnv, "iptables")
	if redirectBackend() != backendIptables {
		t.Fatal("iptables → iptables")
	}
	t.Setenv(RedirectBackendEnv, "bogus")
	if redirectBackend() != backendAuto {
		t.Fatal("unknown → auto")
	}
}

func TestIsIptablesXtExtensionError(t *testing.T) {
	yes := []string{
		"iptables transparent: exit status 4 (output: Warning: Extension owner revision 0 not supported, missing kernel module?\niptables v1.8.11 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n)",
		"ip6tables: No chain/target/match by that name.",
		"Warning: XT target REDIRECT not found",
	}
	for _, m := range yes {
		if !isIptablesXtExtensionError(errors.New(m)) {
			t.Fatalf("expected an xt-extension error for %q", m)
		}
	}
	for _, m := range []string{"", "iptables: permission denied", "exit status 1"} {
		if isIptablesXtExtensionError(errors.New(m)) {
			t.Fatalf("false positive for %q", m)
		}
	}
	if isIptablesXtExtensionError(nil) {
		t.Fatal("nil is not an error")
	}
}
