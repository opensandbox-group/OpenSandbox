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

package mitmproxy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const (
	mitmCACertName = "mitmproxy-ca-cert.pem"
	pollInterval   = 200 * time.Millisecond
	waitCACert     = 20 * time.Second
)

// FleetCAExportDir is the dedicated subdir (under constants.OpenSandboxRootDir)
// where the fleet profile exports the mitm CA. The fastlet bind-mounts this
// directory read-only into every sandbox at creation, so sandbox bootstrap can
// seed the trust store with the CA (OSEP-0022 A1; see fast-sandbox issue #19).
// The directory-level mount (not file-level) keeps the export visible across
// egress's atomic rename-based CA rotation. Deliberately NOT the whole
// OpenSandboxRootDir: a read-only mount of the parent would shadow the
// fastlet-placed execd binary and is shared-writable across sandboxes.
const FleetCAExportDir = "mitm-ca"

// candidateCACertPaths: mitm may place mitmproxy-ca-cert.pem in confdir, .mitmproxy under confdir, or home.
func candidateCACertPaths(confDirEnv, home string) []string {
	confDirEnv = strings.TrimSpace(confDirEnv)
	var out []string
	if confDirEnv != "" {
		out = append(out,
			filepath.Join(confDirEnv, mitmCACertName),
			filepath.Join(confDirEnv, ".mitmproxy", mitmCACertName),
		)
	}
	out = append(out, filepath.Join(home, ".mitmproxy", mitmCACertName))
	return out
}

func waitMitmCACertPath(confDirEnv, home string) (string, error) {
	cands := candidateCACertPaths(confDirEnv, home)
	deadline := time.Now().Add(waitCACert)
	for time.Now().Before(deadline) {
		for _, p := range cands {
			st, err := os.Stat(p)
			if err == nil && !st.IsDir() && st.Size() > 0 {
				return p, nil
			}
		}
		time.Sleep(pollInterval)
	}
	return "", fmt.Errorf("mitmproxy CA not found after %v (tried: %v)", waitCACert, cands)
}

// PurgeStaleExportedCA removes any mitmproxy-ca-cert.pem left on the shared
// volume by a previous egress generation: a restart rotates the CA in the
// ephemeral confdir, and the stale cert would let an agent pass its bootstrap
// readiness check and install a CA mitmproxy no longer signs with (issue #1370).
// The fleet export subdir is purged the same way, so a sandbox holding the
// mount never reads a stale generation's CA (fast-sandbox issue #19).
// Must be called as early as possible in main().
func PurgeStaleExportedCA() {
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return
	}
	purgeStaleExportedCAFrom(constants.OpenSandboxRootDir)
	purgeStaleExportedCAFrom(filepath.Join(constants.OpenSandboxRootDir, FleetCAExportDir))
}

// purgeStaleExportedCAFrom is the testable core of PurgeStaleExportedCA.
func purgeStaleExportedCAFrom(rootDir string) {
	path := filepath.Join(rootDir, mitmCACertName)
	err := os.Remove(path)
	switch {
	case err == nil:
		// warn: on first pod startup this file should not exist; its presence
		// means the egress container (or process) has restarted, which is
		// useful signal when diagnosing HTTPS-from-agent problems.
		log.Warnf("[mitmproxy] removed stale exported CA at %s (previous egress generation left it behind; see upstream issue #1370)", path)
	case os.IsNotExist(err):
		// Common on first pod startup.
	default:
		// Non-fatal: SyncRootCA will overwrite the file. But on the race
		// path the agent may still grab the old contents before we do.
		log.Warnf("[mitmproxy] failed to remove stale exported CA at %s: %v", path, err)
	}
}

func SyncRootCA(confDirEnv, home string) error {
	return exportRootCA(confDirEnv, home, constants.OpenSandboxRootDir, true)
}

// SyncRootCAFleet exports the CA into the dedicated fleet subdir (the fastlet
// mount point). The egress container's own system trust store is NOT touched:
// upstream validation uses the public roots, and the CA is only meaningful to
// the sandbox clients downstream.
func SyncRootCAFleet(confDirEnv, home string) error {
	return exportRootCA(confDirEnv, home, filepath.Join(constants.OpenSandboxRootDir, FleetCAExportDir), false)
}

func exportRootCA(confDirEnv, home, rootDir string, installSystemTrust bool) error {
	src, err := waitMitmCACertPath(confDirEnv, home)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rootDir, err)
	}
	dst := filepath.Join(rootDir, mitmCACertName)
	if err := copyFile(src, dst, 0o644); err != nil {
		return fmt.Errorf("copy mitm CA to %s: %w", dst, err)
	}
	log.Infof("[mitmproxy] copied root CA to %s", dst)

	if !installSystemTrust {
		return nil
	}
	if err := installMitmCAInSystemTrust(dst); err != nil {
		return fmt.Errorf("install mitm CA into system trust store: %w", err)
	}
	return nil
}

func installMitmCAInSystemTrust(pemPath string) error {
	if _, err := exec.LookPath("update-ca-certificates"); err != nil {
		return fmt.Errorf("update-ca-certificates not found (install ca-certificates in the egress image): %w", err)
	}
	dir := "/usr/local/share/ca-certificates"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	systemDst := filepath.Join(dir, "opensandbox-mitmproxy-ca.crt")
	if err := copyFile(pemPath, systemDst, 0o644); err != nil {
		return fmt.Errorf("copy CA to %s: %w", systemDst, err)
	}
	out, err := exec.Command("update-ca-certificates").CombinedOutput()
	if err != nil {
		return fmt.Errorf("update-ca-certificates: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Infof("[mitmproxy] egress container: mitm CA added to system trust (update-ca-certificates)")
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+mitmCACertName+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}
