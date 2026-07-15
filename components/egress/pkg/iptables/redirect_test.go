package iptables

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetupRedirectFallsBackToNftWhenIptablesNftOutputAppendFails(t *testing.T) {
	var iptablesCalls [][]string
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			iptablesCalls = append(iptablesCalls, append([]string(nil), args...))
			return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, []netip.Addr{
		netip.MustParseAddr("10.179.156.2"),
		netip.MustParseAddr("fd00::53"),
	})

	require.NoError(t, err)
	require.NotEmpty(t, iptablesCalls)
	setupScript := nftScripts[len(nftScripts)-1]
	require.Contains(t, setupScript, "add table ip opensandbox_dns_redirect")
	require.Contains(t, setupScript, "add table ip6 opensandbox_dns_redirect")
	require.NotContains(t, setupScript, "add table inet opensandbox_dns_redirect")
	require.Contains(t, setupScript, "type nat hook output priority -100; policy accept;")
	require.Contains(t, setupScript, "udp dport 53 ip daddr 10.179.156.2 return")
	require.Contains(t, setupScript, "tcp dport 53 ip6 daddr fd00::53 return")
	require.Contains(t, setupScript, "udp dport 53 redirect to :15353")
	require.Contains(t, setupScript, "tcp dport 53 redirect to :15353")
	require.False(t, strings.Contains(setupScript, "hook prerouting"))
}

func TestSetupRedirectClearsStaleNftFallbackWhenIptablesSucceeds(t *testing.T) {
	var events []string
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			events = append(events, "iptables")
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			events = append(events, "nft")
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Contains(t, nftScripts, "delete table inet opensandbox_dns_redirect\n")
	require.Contains(t, nftScripts, "delete table ip opensandbox_dns_redirect\n")
	require.Contains(t, nftScripts, "delete table ip6 opensandbox_dns_redirect\n")
	require.GreaterOrEqual(t, len(events), 4)
	require.Equal(t, []string{"nft", "nft", "nft", "iptables"}, events[:4])
}

func TestSetupRedirectIgnoresMissingStaleNftFallbackWhenIptablesSucceeds(t *testing.T) {
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			return []byte("Error: Could not process rule: No such file or directory\n" + script), errors.New("exit status 1")
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
}

func TestSetupRedirectIgnoresUnavailableNftCleanupWhenIptablesSucceeds(t *testing.T) {
	var iptablesCalls int
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			iptablesCalls++
			return nil, nil
		},
		runNft: func(_ context.Context, _ string) ([]byte, error) {
			return nil, exec.ErrNotFound
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Equal(t, 8, iptablesCalls)
}

func TestSetupRedirectIgnoresUnsupportedNftCleanupWhenIptablesSucceeds(t *testing.T) {
	var iptablesCalls int
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			iptablesCalls++
			return nil, nil
		},
		runNft: func(_ context.Context, _ string) ([]byte, error) {
			return []byte("Error: Could not process rule: Protocol not supported"), errors.New("exit status 1")
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Equal(t, 8, iptablesCalls)
}

func TestSetupRedirectContinuesNftCleanupAfterUnsupportedFamilyWhenIptablesSucceeds(t *testing.T) {
	var iptablesCalls int
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			iptablesCalls++
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			if strings.Contains(script, "delete table inet ") {
				return []byte("Error: Could not process rule: Protocol not supported"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Equal(t, 8, iptablesCalls)
	require.Contains(t, nftScripts, "delete table inet opensandbox_dns_redirect\n")
	require.Contains(t, nftScripts, "delete table ip opensandbox_dns_redirect\n")
	require.Contains(t, nftScripts, "delete table ip6 opensandbox_dns_redirect\n")
}

func TestSetupRedirectReturnsNftFallbackErrorWhenUnsupportedNftIsRequired(t *testing.T) {
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
		},
		runNft: func(_ context.Context, _ string) ([]byte, error) {
			return []byte("Error: Could not process rule: Protocol not supported"), errors.New("exit status 1")
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "nft DNS redirect fallback failed")
	require.Contains(t, err.Error(), "Protocol not supported")
}

func TestSetupRedirectReturnsNonMissingStaleNftFallbackCleanupError(t *testing.T) {
	var iptablesCalls int
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			iptablesCalls++
			return nil, nil
		},
		runNft: func(_ context.Context, _ string) ([]byte, error) {
			return []byte("Operation not permitted"), errors.New("exit status 1")
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "nft DNS redirect cleanup failed")
	require.Zero(t, iptablesCalls)
}

func TestSetupRedirectRollsBackPartialIptablesRulesBeforeNftFallback(t *testing.T) {
	var iptablesCalls [][]string
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			iptablesCalls = append(iptablesCalls, append([]string(nil), args...))
			if len(iptablesCalls) == 5 {
				return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
			}
			return nil, nil
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.GreaterOrEqual(t, len(iptablesCalls), 9)
	require.Equal(t, "-A", iptablesCalls[0][3])
	require.Equal(t, "-A", iptablesCalls[3][3])
	require.Equal(t, "-D", iptablesCalls[5][3])
	require.Equal(t, "-D", iptablesCalls[8][3])
	require.Equal(t, iptablesCalls[3][4:], iptablesCalls[5][4:])
	require.Equal(t, iptablesCalls[0][4:], iptablesCalls[8][4:])
	require.Contains(t, nftScripts[len(nftScripts)-1], "add table ip opensandbox_dns_redirect")
	require.Contains(t, nftScripts[len(nftScripts)-1], "add table ip6 opensandbox_dns_redirect")
}

func TestSetupRedirectReturnsRollbackErrorBeforeNftFallback(t *testing.T) {
	var iptablesCalls [][]string
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			iptablesCalls = append(iptablesCalls, append([]string(nil), args...))
			switch {
			case len(iptablesCalls) == 3:
				return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
			case args[3] == "-D":
				return []byte("delete failed"), errors.New("exit status 1")
			default:
				return nil, nil
			}
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "iptables rollback failed")
	for _, script := range nftScripts {
		require.NotContains(t, script, "add table inet opensandbox_dns_redirect")
	}
}

func TestSetupRedirectIgnoresAlreadyAbsentRollbackRuleBeforeNftFallback(t *testing.T) {
	var iptablesCalls [][]string
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, args []string) ([]byte, error) {
			iptablesCalls = append(iptablesCalls, append([]string(nil), args...))
			switch {
			case len(iptablesCalls) == 3:
				return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
			case args[3] == "-D":
				return []byte("iptables: Bad rule (does a matching rule exist in that chain?)."), errors.New("exit status 1")
			default:
				return nil, nil
			}
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Contains(t, nftScripts[len(nftScripts)-1], "add table ip opensandbox_dns_redirect")
	require.Contains(t, nftScripts[len(nftScripts)-1], "add table ip6 opensandbox_dns_redirect")
}

func TestSetupRedirectRetriesNftFallbackWithoutDeleteWhenTableIsMissing(t *testing.T) {
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			return []byte("iptables v1.8.9 (nf_tables):  RULE_APPEND failed (No such file or directory): rule in chain OUTPUT\n"), errors.New("exit status 4")
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			if strings.Contains(script, "add table ip opensandbox_dns_redirect") && strings.Contains(script, "delete table ip opensandbox_dns_redirect") {
				return []byte("Error: Could not process rule: No such file or directory\ndelete table ip opensandbox_dns_redirect"), errors.New("exit status 1")
			}
			return nil, nil
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.NoError(t, err)
	require.Len(t, nftScripts, 5)
	failedSetupScript := nftScripts[len(nftScripts)-2]
	retryScript := nftScripts[len(nftScripts)-1]
	require.Contains(t, failedSetupScript, "delete table ip opensandbox_dns_redirect")
	require.NotContains(t, retryScript, "delete table ip opensandbox_dns_redirect")
	require.NotContains(t, retryScript, "delete table ip6 opensandbox_dns_redirect")
	require.Contains(t, retryScript, "add table ip opensandbox_dns_redirect")
	require.Contains(t, retryScript, "add table ip6 opensandbox_dns_redirect")
}

func TestSetupRedirectReturnsOriginalIptablesErrorWhenFallbackIsNotApplicable(t *testing.T) {
	var nftScripts []string
	r := redirectRunner{
		runCommand: func(_ context.Context, _ []string) ([]byte, error) {
			return []byte("permission denied"), errors.New("exit status 4")
		},
		runNft: func(_ context.Context, script string) ([]byte, error) {
			nftScripts = append(nftScripts, script)
			return []byte("Error: Could not process rule: No such file or directory\n" + script), errors.New("exit status 1")
		},
	}

	err := r.setupRedirect(context.Background(), 15353, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "permission denied")
	for _, script := range nftScripts {
		require.NotContains(t, script, "add table ip opensandbox_dns_redirect")
		require.NotContains(t, script, "add table ip6 opensandbox_dns_redirect")
	}
}
