package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupScriptRemovesNativeDNSRedirectFallbackTables(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.Mkdir(binDir, 0o755))
	logPath := filepath.Join(tmpDir, "nft.log")
	orderPath := filepath.Join(tmpDir, "order.log")

	writeExecutable(t, filepath.Join(binDir, "nft"), `#!/bin/sh
printf '%s\n' "$*" >> "`+logPath+`"
cat >> "`+logPath+`"
printf 'nft\n' >> "`+orderPath+`"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "iptables"), `#!/bin/sh
printf 'iptables\n' >> "`+orderPath+`"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "ip6tables"), `#!/bin/sh
printf 'ip6tables\n' >> "`+orderPath+`"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "pkill"), `#!/bin/sh
exit 0
`)

	cmd := exec.Command("sh", "scripts/cleanup.sh")
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	require.NoError(t, err, string(out))
	logBytes, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logText := string(logBytes)
	require.Contains(t, logText, "delete table inet opensandbox_dns_redirect")
	require.Contains(t, logText, "delete table ip opensandbox_dns_redirect")
	require.Contains(t, logText, "delete table ip6 opensandbox_dns_redirect")
	orderBytes, err := os.ReadFile(orderPath)
	require.NoError(t, err)
	order := strings.Split(strings.TrimSpace(string(orderBytes)), "\n")
	require.NotEmpty(t, order)
	require.Equal(t, "nft", order[0])
}

func writeExecutable(t *testing.T, path string, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o755))
}
