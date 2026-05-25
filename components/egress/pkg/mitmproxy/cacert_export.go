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

func SyncRootCA(confDirEnv, home string) error {
	src, err := waitMitmCACertPath(confDirEnv, home)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(constants.OpenSandboxRootDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", constants.OpenSandboxRootDir, err)
	}
	dst := filepath.Join(constants.OpenSandboxRootDir, mitmCACertName)
	if err := copyFile(src, dst, 0o644); err != nil {
		return fmt.Errorf("copy mitm CA to %s: %w", dst, err)
	}
	log.Infof("[mitmproxy] copied root CA to %s", dst)

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
