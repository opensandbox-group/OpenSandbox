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

package iptables

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/stretchr/testify/require"
)

const enforcementKernelTestName = "TestEnforcementAnchorKernelPrivateNS"

// TestEnforcementAnchorKernelPrivateNS reads the real kernel after every
// enforcement reconcile. The parent only starts a child in a fresh network
// namespace; no nft command runs until the child proves it is not in either
// the captured parent namespace or /proc/1's namespace. It covers the OSEP-0029
// integration scenarios: a single-family table deletion is detected and
// restored independently of the other family, an external flush restores every
// anchored table, and foreign tables are never judged, restored, or mutated.
func TestEnforcementAnchorKernelPrivateNS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parent, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^"+enforcementKernelTestName+"$", "-test.v")
		cmd.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=netns", "OPENSANDBOX_NFT_TEST_PARENT_NETNS="+parent)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		require.Contains(t, string(output), "--- PASS: "+enforcementKernelTestName, "%s", output)
		t.Logf("%s", output)
		return
	case "netns":
		parent := os.Getenv("OPENSANDBOX_NFT_TEST_PARENT_NETNS")
		require.NotEmpty(t, parent, "the parent namespace identity must be captured by the opt-in wrapper")
		current, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		actualParent, err := os.Readlink("/proc/" + strconv.Itoa(os.Getppid()) + "/ns/net")
		require.NoError(t, err)
		require.Equal(t, parent, actualParent, "the captured namespace must belong to the actual process parent")
		pidOne, err := os.Readlink("/proc/1/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, parent, current, "the child must not mutate the captured parent namespace")
		require.NotEqual(t, pidOne, current, "the child must not mutate /proc/1's namespace")
		t.Logf("netns guard passed: uid=%d child=%s captured-parent=%s actual-parent=%s proc1=%s", os.Getuid(), current, parent, actualParent, pidOne)
	default:
		t.Skip("kernel enforcement validation not executed: set OPENSANDBOX_NFT_TEST=1 with namespace/nft permissions")
	}

	nftPath, err := exec.LookPath("nft")
	require.NoError(t, err)
	version, err := exec.CommandContext(ctx, nftPath, "--version").CombinedOutput()
	require.NoError(t, err, "%s", version)
	t.Logf("nft version: %s", strings.TrimSpace(string(version)))

	// Native nft backend: the owned tables are what the anchors cover.
	t.Setenv(RedirectBackendEnv, "nft")
	flushRuleset(t, ctx)
	t.Cleanup(func() { flushRuleset(t, context.Background()) })

	ledger := nftables.NewAnchorLedger(nftables.SystemReadbackRunner())
	reconcile := func() { ledger.ReconcileOnce(context.Background()) }

	require.NoError(t, SetupRedirectAnchored(ledger, 15353, nil))
	require.NoError(t, SetupTransparentHTTPAnchored(ledger, 18081, 0, "80,443"))
	assertAllConfirmed(t, ledger, 4, "both family pairs must anchor as confirmed at install")

	// A second pass over an untouched kernel is a no-op restore.
	reconcile()
	assertAllConfirmed(t, ledger, 4, "a matching table must stay confirmed without re-apply")

	// Foreign ruleset content is never judged, restored, or mutated.
	runNft(t, ctx, "add table inet opensandbox_foreign")
	runNft(t, ctx, "add rule inet opensandbox_foreign output meta mark 1 return")
	t.Cleanup(func() {
		runNftQuiet(context.Background(), "delete table inet opensandbox_foreign")
	})
	reconcile()
	assertAllConfirmed(t, ledger, 4, "foreign content must not disturb the anchors")
	requireKernelTableExists(t, ctx, "inet", "opensandbox_foreign", "the foreign table must survive a reconcile")

	// One address-family table deleted externally: detected and restored
	// independently — the ip table stays confirmed the whole time.
	runNft(t, ctx, "delete table ip6 "+DNSRedirectNftTable)
	requireKernelTableExists(t, ctx, "ip", DNSRedirectNftTable, "the untouched family must still be present before the reconcile")
	requireKernelTableAbsent(t, ctx, "ip6", DNSRedirectNftTable)
	reconcile()
	requireKernelTableExists(t, ctx, "ip6", DNSRedirectNftTable, "the deleted family must be restored within one reconcile")
	requireKernelTableExists(t, ctx, "ip", DNSRedirectNftTable, "restoring one family must not disturb the other")
	assertAllConfirmed(t, ledger, 4, "both families must report confirmed after the single-family restore")

	// An external flush of the whole ruleset: every anchored table is
	// restored, and only the anchored ones.
	runNft(t, ctx, "flush ruleset")
	for _, family := range []string{"ip", "ip6"} {
		requireKernelTableAbsent(t, ctx, family, DNSRedirectNftTable)
		requireKernelTableAbsent(t, ctx, family, MitmRedirectNftTable)
	}
	reconcile()
	for _, family := range []string{"ip", "ip6"} {
		requireKernelTableExists(t, ctx, family, DNSRedirectNftTable, "the flushed DNS redirect must be restored")
		requireKernelTableExists(t, ctx, family, MitmRedirectNftTable, "the flushed MITM redirect must be restored")
	}
	// A foreign table flushed from the kernel must not be recreated.
	requireKernelTableAbsent(t, ctx, "inet", "opensandbox_foreign")
	assertAllConfirmed(t, ledger, 4, "all anchored tables must report confirmed after the full restore")
}

func assertAllConfirmed(t *testing.T, ledger *nftables.AnchorLedger, want int, msg string) {
	t.Helper()
	records := ledger.Enforcement()
	require.Len(t, records, want, msg)
	for _, record := range records {
		require.Equal(t, nftables.EnforcementConfirmed, record.State, "%s %s: %s (%s)", record.Family, record.Table, record.State, record.LastError)
		require.Zero(t, record.ConsecutiveFailures, msg)
	}
}

func runNft(t *testing.T, ctx context.Context, script string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script + "\n")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "nft %q failed: %s", script, output)
}

func runNftQuiet(ctx context.Context, script string) {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script + "\n")
	_, _ = cmd.CombinedOutput()
}

func flushRuleset(t *testing.T, ctx context.Context) {
	t.Helper()
	runNft(t, ctx, "flush ruleset")
}

func requireKernelTableExists(t *testing.T, ctx context.Context, family, table, msg string) {
	t.Helper()
	output, err := listKernelTable(ctx, family, table)
	require.NoError(t, err, "%s: %s %s", msg, family, table)
	require.Contains(t, string(output), "table "+family+" "+table, "the listing must identify the requested table")
}

func requireKernelTableAbsent(t *testing.T, ctx context.Context, family, table string) {
	t.Helper()
	output, err := listKernelTable(ctx, family, table)
	require.Error(t, err, "%s %s must be absent", family, table)
	require.Contains(t, strings.ToLower(string(output)), "no such file or directory", "absence must be ENOENT, not a command failure: %s", output)
}

func listKernelTable(ctx context.Context, family, table string) ([]byte, error) {
	return exec.CommandContext(ctx, "nft", "-n", "list", "table", family, table).CombinedOutput()
}
