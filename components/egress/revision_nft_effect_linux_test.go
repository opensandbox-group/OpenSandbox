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

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

func TestRevisionNftEffectsRealFileAndKernel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parent, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestRevisionNftEffectsRealFileAndKernel$", "-test.v")
		cmd.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=netns", "OPENSANDBOX_NFT_TEST_PARENT_NETNS="+parent)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		t.Logf("%s", out)
		return
	case "netns":
		parent := os.Getenv("OPENSANDBOX_NFT_TEST_PARENT_NETNS")
		require.NotEmpty(t, parent)
		current, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, parent, current)
	default:
		t.Skip("kernel validation not executed: set OPENSANDBOX_NFT_TEST=1 with namespace/nft permissions")
	}
	for _, exists := range []bool{false, true} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("exists=%v-unknown=%v", exists, unknown), func(t *testing.T) {
				s := recoveryPolicyFixture(t)
				dir := t.TempDir()
				require.NoError(t, os.Chmod(dir, 0700))
				s.policyFile = filepath.Join(dir, "policy.json")
				original := []byte("  { \"defaultAction\": \"deny\" } \n")
				if exists {
					require.NoError(t, os.WriteFile(s.policyFile, original, 0640))
				}
				opts := nftables.Options{QuiesceOnApplyFailure: true}
				production := nftables.NewManagerWithOptions(opts)
				old, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"},{"action":"allow","target":"198.51.100.10"}]}`)
				require.NoError(t, err)
				require.NoError(t, production.ApplyStatic(ctx, old))
				require.NoError(t, production.AddResolvedDomain(ctx, "api.example.com", quiescenceIPs()))
				readKernel := func() map[string]any {
					out, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "opensandbox").CombinedOutput()
					require.NoError(t, err, "%s", out)
					// Dynamic expiry fields decrease with wall time; compare actual sets and
					// rules after removing only those expiry counters from nft's JSON.
					var doc map[string]any
					require.NoError(t, json.Unmarshal(out, &doc))
					var scrub func(any)
					scrub = func(v any) {
						switch x := v.(type) {
						case map[string]any:
							delete(x, "expires")
							delete(x, "handle")
							for _, child := range x {
								scrub(child)
							}
						case []any:
							for _, child := range x {
								scrub(child)
							}
						}
					}
					scrub(doc)
					return doc
				}
				before := readKernel()
				oldBase, oldProxy := s.revisionRecovery.current, s.proxy.CurrentPolicy()
				_, _, ticket, err := s.captureRevisionBootstrap(ctx)
				require.NoError(t, err)
				path := os.Getenv("PATH")
				if unknown {
					s.nft = nftables.NewManagerWithRunnerAndOptions(func(ctx context.Context, script string) ([]byte, error) {
						cmd := exec.CommandContext(ctx, "nft", "-f", "-")
						cmd.Stdin = strings.NewReader(script)
						out, err := cmd.CombinedOutput()
						if err != nil {
							return out, fmt.Errorf("real nft failed: %w (%s)", err, out)
						}
						return out, fmt.Errorf("injected lost result after real commit")
					}, opts)
				} else {
					s.nft = production
					t.Setenv("PATH", t.TempDir())
				}
				w := applyEffectRequest(s, http.MethodPost)
				t.Setenv("PATH", path)
				require.Equal(t, 500, w.Code, w.Body.String())
				require.Same(t, oldBase, s.revisionRecovery.current)
				require.Same(t, oldProxy, s.proxy.CurrentPolicy())
				data, err := os.ReadFile(s.policyFile)
				if exists {
					require.NoError(t, err)
					require.Equal(t, original, data)
					info, err := os.Stat(s.policyFile)
					require.NoError(t, err)
					require.Equal(t, os.FileMode(0640), info.Mode().Perm())
				} else {
					require.True(t, os.IsNotExist(err), "old absence must be durably restored")
				}
				if unknown {
					require.True(t, s.revisionRecoveryRequired.Load())
					require.Equal(t, 503, recoveryHealthzProbe(s).Code)
					require.ErrorIs(t, validateRecoveryTicket(s, ticket), errRevisionRecoveryRequired)
					frozen := readKernel()
					require.NotEqual(t, before, frozen, "injection must follow a real kernel commit")
					assertQuiescenceRejectsWrites(t, s.nft.(*nftables.Manager))
					require.Equal(t, frozen, readKernel())
				} else {
					require.False(t, s.revisionRecoveryRequired.Load())
					require.Equal(t, 200, recoveryHealthzProbe(s).Code)
					require.Equal(t, before, readKernel(), "real Start failure must preserve static and dynamic rules")
					require.ErrorIs(t, validateRecoveryTicket(s, ticket), errStaleRevisionBootstrap)
					require.Equal(t, 200, applyEffectRequest(s, http.MethodPost).Code)
					require.NotEqual(t, before, readKernel())
					require.NotSame(t, oldBase, s.revisionRecovery.current)
					data, err = os.ReadFile(s.policyFile)
					require.NoError(t, err)
					require.Contains(t, string(data), "allow")
				}
			})
		}
	}
	// This owner case executes nft in the private namespace above. Receiver
	// failure is injected after confirmed kernel commit; packet-level fencing
	// remains covered separately by the quarantine cross-namespace tests.
	for _, receiver := range []string{"reject", "unknown"} {
		t.Run("receiver-after-kernel-commit-"+receiver, func(t *testing.T) {
			s, m, session, _ := revisionPolicyFixture(t)
			s.policyFile = filepath.Join(t.TempDir(), "policy.json")
			original := []byte(`{"defaultAction":"deny"}`)
			require.NoError(t, os.WriteFile(s.policyFile, original, 0600))
			old := s.revisionRecovery.current
			manager := nftables.NewManagerWithOptions(nftables.Options{QuiesceOnApplyFailure: true})
			s.nft = manager
			require.NoError(t, manager.ApplyStatic(ctx, old.inputs.user))
			before, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "opensandbox").CombinedOutput()
			require.NoError(t, err, "%s", before)
			fence := runtimeQuarantineFixture(s)
			stops := 0
			m.revisionOwner.stop = func(*mitmproxy.Running) { stops++ }
			var committed []byte
			session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
				var err error
				committed, err = exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "opensandbox").CombinedOutput()
				require.NoError(t, err, "%s", committed)
				require.NotEqual(t, before, committed, "receiver attempt must follow actual changed kernel rules")
				if receiver == "reject" {
					return revision.Identity{}, revision.ErrPrepareRejected
				}
				return mutationIdentity(t, snapshot, epoch), revision.ErrIndeterminate
			}
			session.reconcile = func(context.Context, revision.Identity) (bool, error) { return false, revision.ErrClosed }
			response := revisionPolicyDo(t, s, http.MethodPost, `{"defaultAction":"allow"}`)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Same(t, old, s.revisionRecovery.current)
			require.True(t, s.revisionRecoveryRequired.Load())
			require.Equal(t, 1, stops)
			require.Equal(t, 1, session.closeCalls)
			require.Equal(t, []string{"namespace", "ensure", "namespace"}, fence.calls)
			after, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "opensandbox").CombinedOutput()
			require.NoError(t, err, "%s", after)
			require.Equal(t, committed, after, "old policy must not be replayed as a healthy rollback")
			assertQuiescenceRejectsWrites(t, manager)
			require.Equal(t, http.StatusServiceUnavailable, revisionPolicyDo(t, s, http.MethodDelete, `["missing.example.com"]`).Code)
			t.Log("real nft commit and sticky owner cleanup observed; packet isolation requires separate privileged quarantine tests")
		})
	}

}

func TestRevisionNftUnchangedRealRestore(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			s := recoveryPolicyFixture(t)
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			s.policyFile = filepath.Join(dir, "policy.json")
			original := []byte("  { \"defaultAction\": \"deny\" } \n")
			if exists {
				require.NoError(t, os.WriteFile(s.policyFile, original, 0640))
			}
			n := &stubNft{err: &nftables.ApplyError{Effect: nftables.ApplyUnchanged, Err: fmt.Errorf("start failed")}}
			s.nft = n
			old := s.revisionRecovery.current
			require.Equal(t, 500, applyEffectRequest(s, http.MethodPost).Code)
			require.False(t, s.revisionRecoveryRequired.Load())
			require.Zero(t, n.quiesces)
			require.Same(t, old, s.revisionRecovery.current)
			data, err := os.ReadFile(s.policyFile)
			if exists {
				require.NoError(t, err)
				require.Equal(t, original, data)
			} else {
				require.True(t, os.IsNotExist(err))
			}
			n.err = nil
			require.Equal(t, 200, applyEffectRequest(s, http.MethodPost).Code)
			require.NotSame(t, old, s.revisionRecovery.current)
		})
	}
}
