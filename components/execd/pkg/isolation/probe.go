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

package isolation

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ProbeResult holds the result of startup isolation probing.
type ProbeResult struct {
	Available        bool
	Isolator         string
	Version          string
	CommitSupported  bool // Phase 2
	DiffSupported    bool // Phase 2
	PersistAvailable bool // Phase 2 — requires emptyDir
}

// ProbeConfig controls Probe behaviour.
type ProbeConfig struct {
	UpperRoot     string
	UpperMaxBytes int64
}

// Probe runs startup detection. Returns a ProbeResult describing what
// isolation capabilities are available in the current environment.
//
// On Linux with working bwrap:
//
//	Available=true, Isolator="bwrap", Version="0.10.0"
//
// Otherwise:
//
//	Available=false
func Probe(cfg ProbeConfig) ProbeResult {
	result := ProbeResult{}

	// Check if bwrap binary is available.
	version, err := probeBwrapVersion()
	if err != nil {
		return result
	}

	result.Available = true
	result.Isolator = "bwrap"
	result.Version = version

	// Smoke test: verify bwrap can actually create a namespace.
	if err := probeBwrapSmoke(); err != nil {
		result.Available = false
		return result
	}

	// Phase 2: probe commit support (overlay mount).
	// result.CommitSupported = probeOverlayMount()

	return result
}

// probeBwrapVersion returns the bwrap version string if available.
func probeBwrapVersion() (string, error) {
	p := findBwrap()
	if p == "" {
		return "", fmt.Errorf("bwrap not found")
	}

	var stderr bytes.Buffer
	cmd := exec.Command(p, "--version")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	// bwrap v0.8+ prints version to stderr, e.g.:
	// "bubblewrap 0.8.0" or "bwrap 0.10.0"
	out := stderr.String()
	return parseBwrapVersion(out), nil
}

var bwrapVersionRe = regexp.MustCompile(`b(?:ubble)?wrap\s+(\d+\.\d+\.\d+)`)

// parseBwrapVersion extracts the version number from bwrap --version output.
func parseBwrapVersion(out string) string {
	match := bwrapVersionRe.FindStringSubmatch(out)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// probeBwrapSmoke verifies bwrap can create a minimal namespace.
func probeBwrapSmoke() error {
	p := findBwrap()
	if p == "" {
		return fmt.Errorf("bwrap not found")
	}
	cmd := exec.Command(p,
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--", "true",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bwrap smoke test failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// probeOverlayMount is deferred to Phase 2.
