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
	"net/netip"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingAnchors captures the anchors the redirect appliers record.
type recordingAnchors struct {
	anchors map[string]func(context.Context) error
	dropped []string
}

func newRecordingAnchors() *recordingAnchors {
	return &recordingAnchors{anchors: map[string]func(context.Context) error{}}
}

func (r *recordingAnchors) AnchorTable(_ context.Context, family, table string, reapply func(context.Context) error) error {
	r.anchors[family+"/"+table] = reapply
	return nil
}

func (r *recordingAnchors) DropTable(family, table string) {
	r.dropped = append(r.dropped, family+"/"+table)
}

func (r *recordingAnchors) keys() []string {
	keys := make([]string, 0, len(r.anchors))
	for key := range r.anchors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestSetupRedirectAnchorsBothFamiliesOnNftBackend covers the sidecar's
// native-nft path: both family tables are recorded, and the restore closure
// replays the same transactional install.
func TestSetupRedirectAnchorsBothFamiliesOnNftBackend(t *testing.T) {
	t.Setenv(RedirectBackendEnv, "nft")
	var scripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			t.Fatalf("the nft backend must not run iptables: %v", args)
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			scripts = append(scripts, script)
			return nil, nil
		},
	}
	anchors := newRecordingAnchors()
	require.NoError(t, r.setupRedirect(context.Background(), anchors, 15353, []netip.Addr{netip.MustParseAddr("10.0.0.53")}))

	require.Equal(t, []string{"ip/opensandbox_dns_redirect", "ip6/opensandbox_dns_redirect"}, anchors.keys())

	// The reapply closure must run the same whole-transaction install that
	// created the table, so drift restoration converges on the install path.
	before := len(scripts)
	require.NoError(t, anchors.anchors["ip6/opensandbox_dns_redirect"](context.Background()))
	require.Len(t, scripts, before+1)
	replay := scripts[len(scripts)-1]
	require.Contains(t, replay, "delete table ip6 opensandbox_dns_redirect")
	require.Contains(t, replay, "add table ip6 opensandbox_dns_redirect")
	require.Contains(t, replay, "ip daddr 10.0.0.53")
}

// TestSetupRedirectRecordsNoAnchorOnIptablesBackend: the default backend
// appends rules to the shared nat OUTPUT chain — there is no owned table to
// read back, so nothing is anchored (deletion detection there is out of
// scope, OSEP-0029).
func TestSetupRedirectRecordsNoAnchorOnIptablesBackend(t *testing.T) {
	t.Setenv(RedirectBackendEnv, "iptables")
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}
	anchors := newRecordingAnchors()
	require.NoError(t, r.setupRedirect(context.Background(), anchors, 15353, nil))
	require.Empty(t, anchors.keys())
	for _, script := range nftScripts {
		require.NotContains(t, script, "add table", "the iptables backend must not install an owned table")
	}
}

// TestSetupRedirectAnchorRecordingFailureDoesNotFailInstall: the install
// already committed; a capture failure must be logged, never propagated.
func TestSetupRedirectAnchorRecordingFailureDoesNotFailInstall(t *testing.T) {
	t.Setenv(RedirectBackendEnv, "nft")
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) { return nil, nil },
		runNft:     func(_ context.Context, script string) ([]byte, error) { return nil, nil },
	}
	failing := failingAnchors{err: errors.New("snapshot failed")}
	require.NoError(t, r.setupRedirect(context.Background(), failing, 15353, nil))
}

type failingAnchors struct{ err error }

func (f failingAnchors) AnchorTable(context.Context, string, string, func(context.Context) error) error {
	return f.err
}

func (failingAnchors) DropTable(string, string) {}

// TestAnchorTransparentRedirectRecordsBothFamilies covers the transparent-MITM
// table wiring (the installed tables themselves are covered by the kernel
// test).
func TestAnchorTransparentRedirectRecordsBothFamilies(t *testing.T) {
	var scripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) { return nil, nil },
		runNft: func(_ context.Context, script string) ([]byte, error) {
			scripts = append(scripts, script)
			return nil, nil
		},
	}
	anchors := newRecordingAnchors()
	anchorTransparentRedirect(context.Background(), anchors, r, 18081, 10042, "80,443")
	require.Equal(t, []string{"ip/opensandbox_mitm_redirect", "ip6/opensandbox_mitm_redirect"}, anchors.keys())
	require.NoError(t, anchors.anchors["ip/opensandbox_mitm_redirect"](context.Background()))
	require.NotEmpty(t, scripts)
	require.True(t, strings.Contains(scripts[len(scripts)-1], "add table ip opensandbox_mitm_redirect"))
}

// TestAnchorRedirectTableNilLedgerIsNoOp pins the default-off behavior.
func TestAnchorRedirectTableNilLedgerIsNoOp(t *testing.T) {
	anchorRedirectTable(context.Background(), nil, "ip", "opensandbox_dns_redirect", func(context.Context) error {
		t.Fatal("a nil ledger must not invoke a restore path")
		return nil
	})
	dropRedirectAnchor(nil, "ip", "opensandbox_dns_redirect")
}
