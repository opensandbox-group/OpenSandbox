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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// ProbeResult holds the result of startup isolation probing.
type ProbeResult struct {
	Available        bool
	Isolator         string
	Version          string
	Message          string // diagnostic message when unavailable
	SetprivAvailable bool   // default setpriv uid mode can create the required namespaces
	// SetprivSwitchAvailable is an internal preflight capability for
	// requests that choose IDs different from the execd process IDs. The public
	// setpriv_available flag intentionally describes the default identity path.
	SetprivSwitchAvailable bool
	UsernsAvailable        bool // userns uid mode can create the required namespaces
	CommitSupported        bool // Phase 2
	DiffSupported          bool // Phase 2
	PersistAvailable       bool // Phase 2 — requires emptyDir
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
		result.Message = fmt.Sprintf("bwrap not found: %v (searched: $PATH, /opt/opensandbox/bwrap, /usr/bin/bwrap, /usr/local/bin/bwrap)", err)
		log.Warn("isolation probe: %s", result.Message)
		return result
	}

	result.Isolator = "bwrap"
	result.Version = version

	// Probe each uid mode independently. Some environments allow an
	// unprivileged user namespace but do not grant the capabilities required
	// by setpriv mode (or vice versa), so one failing mode must not disable the
	// other.
	setprivErr := probeBwrapSetprivSmoke()
	setprivIdentitySwitchErr := setprivErr
	if setprivErr == nil {
		setprivIdentitySwitchErr = probeBwrapSetprivIdentitySwitchSmoke()
	}
	usernsErr := probeBwrapUsernsSmoke()
	setBwrapModeAvailability(&result, setprivErr, setprivIdentitySwitchErr, usernsErr)
	if setprivErr != nil {
		log.Warn("isolation probe: setpriv uid mode unavailable: %v", setprivErr)
	} else if setprivIdentitySwitchErr != nil {
		log.Warn("isolation probe: setpriv uid mode cannot switch to arbitrary uid/gid: %v", setprivIdentitySwitchErr)
	}
	if usernsErr != nil {
		log.Warn("isolation probe: userns uid mode unavailable: %v", usernsErr)
	}
	if !result.Available {
		result.Message = fmt.Sprintf(
			"bwrap found (v%s) but no uid mode is available (setpriv: %v; userns: %v)",
			version, setprivErr, usernsErr,
		)
		log.Warn("isolation probe: %s", result.Message)
		return result
	}

	if probeOverlayMount(cfg.UpperRoot) {
		result.CommitSupported = true
		result.DiffSupported = true
	}

	return result
}

func setBwrapModeAvailability(result *ProbeResult, setprivErr, setprivIdentitySwitchErr, usernsErr error) {
	result.SetprivAvailable = setprivErr == nil
	result.SetprivSwitchAvailable = setprivErr == nil && setprivIdentitySwitchErr == nil
	result.UsernsAvailable = usernsErr == nil
	result.Available = result.SetprivAvailable || result.UsernsAvailable
}

// probeBwrapVersion returns the bwrap version string if available.
func probeBwrapVersion() (string, error) {
	p := findBwrap()
	if p == "" {
		return "", fmt.Errorf("bwrap not found")
	}

	var stdout bytes.Buffer
	cmd := exec.Command(p, "--version")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}

	// bwrap prints version to stdout, e.g.:
	// "bubblewrap 0.8.0" or "bwrap 0.10.0"
	out := stdout.String()
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

// probeBwrapSetprivSmoke verifies the exact default setpriv path. A root execd
// with omitted uid/gid does not invoke setpriv, while a non-root execd invokes
// setpriv with its current IDs.
func probeBwrapSetprivSmoke() error {
	uid, gid := currentProcessIDs()
	return probeBwrapSmoke(UidModeSetpriv, uid, gid)
}

// probeBwrapSetprivIdentitySwitchSmoke verifies that setpriv can switch to IDs
// different from execd's own. Runtime uses this result only for requests that
// explicitly require such a switch, so a default root session is not rejected
// merely because a minimal image omits setpriv.
func probeBwrapSetprivIdentitySwitchSmoke() error {
	uid, gid := currentProcessIDs()
	uid, gid = setprivSmokeTargetIDs(uid, gid)
	return probeBwrapSmoke(UidModeSetpriv, uid, gid)
}

// probeBwrapUsernsSmoke verifies bwrap can create the user namespace and apply
// the uid/gid mapping used by the userns uid mode.
func probeBwrapUsernsSmoke() error {
	uid, gid := currentProcessIDs()
	return probeBwrapSmoke(UidModeUserns, uid, gid)
}

func probeBwrapSmoke(mode UidMode, uid, gid uint32) error {
	p := findBwrap()
	if p == "" {
		return fmt.Errorf("bwrap not found")
	}

	cmd := exec.Command(p, bwrapSmokeArgs(mode, isSetuidBinary(p), uid, gid)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bwrap %s smoke test failed: %w (stderr: %s)", mode, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// setprivSmokeTargetIDs returns non-zero IDs different from the process IDs.
// That makes the smoke test exercise the CAP_SETUID/CAP_SETGID path required
// for arbitrary uid/gid requests instead of merely re-applying the current
// identity, which can succeed without those capabilities.
func setprivSmokeTargetIDs(currentUID, currentGID uint32) (uint32, uint32) {
	const unprivilegedID uint32 = 65534
	targetUID, targetGID := unprivilegedID, unprivilegedID
	if currentUID == targetUID {
		targetUID--
	}
	if currentGID == targetGID {
		targetGID--
	}
	return targetUID, targetGID
}

func bwrapSmokeArgs(mode UidMode, setuidBwrap bool, uid, gid uint32) []string {
	var args []string
	if mode == UidModeUserns {
		args = append(args, "--unshare-user")
		if !setuidBwrap {
			args = append(args, "--disable-userns")
		}
	}
	args = append(args,
		"--unshare-pid", "--unshare-uts", "--unshare-ipc", "--unshare-cgroup",
	)
	if mode == UidModeUserns {
		args = append(args,
			"--uid", strconv.FormatUint(uint64(uid), 10),
			"--gid", strconv.FormatUint(uint64(gid), 10),
		)
	}
	args = append(args,
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--",
	)
	// Match buildArgv: root sessions that keep uid/gid 0 do not invoke
	// setpriv, while any non-zero effective ID uses the identity helper.
	if mode == UidModeSetpriv && (uid != 0 || gid != 0) {
		args = append(args,
			"setpriv",
			fmt.Sprintf("--reuid=%d", uid),
			fmt.Sprintf("--regid=%d", gid),
			"--clear-groups",
		)
	}
	return append(args, "true")
}

// probeOverlayMount tests whether bwrap can create an overlay mount.
func probeOverlayMount(upperRoot string) bool {
	p := findBwrap()
	if p == "" {
		return false
	}

	// Probe on the upper root filesystem (typically tmpfs/emptyDir) rather
	// than /tmp, because overlayfs cannot nest on Docker's overlay2 layer
	// but works fine on tmpfs.
	base := upperRoot
	if base == "" {
		base = os.TempDir()
	}
	tmpDir, err := os.MkdirTemp(base, "execd-probe-overlay-*")
	if err != nil {
		log.Warn("isolation probe: overlay: MkdirTemp(%s): %v", base, err)
		return false
	}
	defer os.RemoveAll(tmpDir)

	lowerDir := filepath.Join(tmpDir, "lower")
	upperDir := filepath.Join(tmpDir, "upper")
	workDir := filepath.Join(tmpDir, "work")
	for _, d := range []string{lowerDir, upperDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Warn("isolation probe: overlay: MkdirAll(%s): %v", d, err)
			return false
		}
	}

	cmd := exec.Command(p,
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--overlay-src", lowerDir,
		"--overlay", upperDir, workDir, "/mnt",
		"--", "true",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Warn("isolation probe: overlay mount failed: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		return false
	}
	return true
}
