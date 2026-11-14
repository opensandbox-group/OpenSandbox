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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/fastsandboxnft"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

// Uses the existing opt-in private-netns test convention. This validates real
// accumulated kernel rules through the production action caller; it does not
// claim packet-forwarding, DNAT, or bridge-topology coverage.
func TestTransitionRealNftChangedIPKernelRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parent, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^TestTransitionRealNftChangedIPKernelRules$", "-test.v")
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
		t.Skip("kernel IP-reuse validation not executed: set OPENSANDBOX_NFT_TEST=1 with namespace/nft permissions")
	}
	_, err := exec.LookPath("nft")
	require.NoError(t, err)
	srv, reg, _ := fastSandboxTestServer(t)
	srv.nft = fastsandboxnft.NewApplier(nil, fastsandboxnft.Options{MitmRedirectPort: 18080})
	allow := `{"defaultAction":"allow"}`
	setBindingAndReady(t, srv, reg, testUID, testIP, &allow)
	unrelated := setBindingAndReady(t, srv, reg, "unrelated", "10.0.0.9", &allow)
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
	require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "successor", testIP, "C", "C-att", 1, vaultAllowPolicy())).Code)
	require.Equal(t, subject.StateActive, mustState(reg.Get(unrelated)))
	for _, chain := range []string{"dispatch", "marking", "input"} {
		out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", fastsandboxnft.TableName, chain).CombinedOutput()
		require.NoError(t, err, "%s", out)
		var reused []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "ip saddr "+testIP+" ") && strings.Contains(line, " jump ") {
				reused = append(reused, line)
			}
		}
		require.Len(t, reused, 1, "actual kernel %s chain must have exactly one reused-IP owner: %s", chain, out)
		require.Contains(t, reused[0], "s_successor")
		require.Contains(t, string(out), "ip saddr 10.0.0.6")
		require.Contains(t, string(out), "ip saddr 10.0.0.9")
	}
	out, err := exec.CommandContext(ctx, "nft", "list", "chain", "inet", fastsandboxnft.TableName, "mark_s_unrelated").CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Regexp(t, `meta mark set (0x0*2|2)\b`, string(out))
}
