//go:build linux

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
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// bwrapPath is the path to the bwrap binary. It is discovered at startup by
// findBwrap and cached for subsequent use.
var bwrapPath string

// seccompBPF holds pre-generated seccomp BPF bytecode, initialised once at
// startup by generateSeccompDenyBPF.
var seccompBPF []byte

// findBwrap locates the bwrap binary. Priority order:
//
//  1. $PATH lookup                 — respect user-installed bwrap
//  2. /opt/opensandbox/bwrap       — injected by init container alongside execd
//  3. /usr/bin/bwrap               — system package (Alpine apk)
//  4. /usr/local/bin/bwrap         — manual install
func findBwrap() string {
	// First: respect whatever the user has in $PATH.
	if path, err := exec.LookPath("bwrap"); err == nil {
		return path
	}
	// Fall back to known locations.
	for _, p := range []string{
		"/opt/opensandbox/bwrap",
		"/usr/bin/bwrap",
		"/usr/local/bin/bwrap",
	} {
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	return ""
}

// bwrapIsSetuid reports whether the resolved bwrap binary has the setuid bit
// set. The setuid build of bubblewrap does not support --disable-userns, so
// buildArgv must skip that flag in userns mode. Detected once at startup.
var bwrapIsSetuid bool

// isSetuidBinary reports whether the file at path has the setuid bit set.
func isSetuidBinary(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSetuid != 0
}

func currentProcessIDs() (uint32, uint32) {
	return uint32(os.Getuid()), uint32(os.Getgid())
}

// bwrapImpl is the Linux bwrap Isolator.
type bwrapImpl struct {
	probe ProbeResult
}

// NewBwrap returns a bwrap Isolator for Linux, configured by cfg.
func NewBwrap(cfg Config) Isolator {
	probe := Probe(ProbeConfig{
		UpperRoot:     cfg.UpperRoot,
		UpperMaxBytes: cfg.UpperMaxBytes,
	})
	return NewBwrapWithProbe(cfg, probe)
}

// NewBwrapWithProbe returns a bwrap Isolator using an existing startup probe.
// It avoids repeating namespace smoke tests when the caller already probed.
func NewBwrapWithProbe(cfg Config, probe ProbeResult) Isolator {
	bwrapPath = findBwrap()
	bwrapIsSetuid = isSetuidBinary(bwrapPath)

	// Pre-generate seccomp BPF once at startup.
	if bpf, err := generateSeccompDenyBPF(cfg.Seccomp); err != nil {
		log.Warn("seccomp: failed to generate BPF: %v", err)
	} else {
		seccompBPF = bpf
	}

	return &bwrapImpl{probe: probe}
}

func (b *bwrapImpl) Name() string { return "bwrap" }

func (b *bwrapImpl) Available() bool {
	if !b.probe.Available {
		return false
	}
	gate, err := openSessionGate()
	if err != nil {
		return false
	}
	_ = gate.Close()
	return true
}

func (b *bwrapImpl) Capabilities() Capabilities {
	return Capabilities{
		Available:              b.Available(),
		Isolator:               b.probe.Isolator,
		Version:                b.probe.Version,
		SetprivAvailable:       b.probe.SetprivAvailable,
		SetprivSwitchAvailable: b.probe.SetprivSwitchAvailable,
		UsernsAvailable:        b.probe.UsernsAvailable,
		Profiles:               []Profile{ProfileStrict, ProfileBalanced},
		ShareNetOverridable:    true,
		CommitSupported:        false, // Phase 2
		DiffSupported:          false, // Phase 2
		PersistAvailable:       false, // Phase 2
		PersistMaxBytesDefault: 2 * 1024 * 1024 * 1024,
		PersistMaxBytesLimit:   8 * 1024 * 1024 * 1024,
		PersistRetainDefault:   3600,
	}
}

func (b *bwrapImpl) Wrap(cmd *exec.Cmd, opts WrapOptions) error {
	if bwrapPath == "" {
		bwrapPath = findBwrap()
	}
	if bwrapPath == "" {
		return fmt.Errorf("bwrap: binary not found")
	}

	firstAddedFile := len(cmd.ExtraFiles)
	seccompFd, err := appendSeccompFile(cmd)
	if err != nil {
		return err
	}

	argv, err := buildArgv(opts, seccompFd)
	if err != nil {
		closeExtraFilesFrom(cmd, firstAddedFile)
		return fmt.Errorf("bwrap: %w", err)
	}

	wrapWithArgv(cmd, bwrapPath, argv)
	return nil
}

// WrapWithLifecycle installs the native fail-closed workload gate and exposes
// bubblewrap's host-visible child/network identity. The returned lifecycle
// must be aborted and closed if cmd.Start is not called. Once cmd.Start
// returns, the caller must close the parent copies in cmd.ExtraFiles.
func (b *bwrapImpl) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts WrapOptions,
) (WorkloadLifecycle, error) {
	if bwrapPath == "" {
		bwrapPath = findBwrap()
	}
	if bwrapPath == "" {
		return nil, fmt.Errorf("bwrap: binary not found")
	}
	if err := validateWrapOptions(opts); err != nil {
		return nil, fmt.Errorf("bwrap: %w", err)
	}

	firstAddedFile := len(cmd.ExtraFiles)
	cleanupExtraFiles := true
	defer func() {
		if cleanupExtraFiles {
			closeExtraFilesFrom(cmd, firstAddedFile)
		}
	}()

	seccompFD, err := appendSeccompFile(cmd)
	if err != nil {
		return nil, err
	}

	gateFile, err := openSessionGate()
	if err != nil {
		return nil, fmt.Errorf("bwrap: %w", err)
	}
	gateExecFD := appendExtraFile(cmd, gateFile)

	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("bwrap: create status pipe: %w", err)
	}
	parentFiles := []*os.File{statusReader}
	defer func() {
		if cleanupExtraFiles {
			for _, file := range parentFiles {
				_ = file.Close()
			}
		}
	}()
	statusFD := appendExtraFile(cmd, statusWriter)

	setupReader, setupWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("bwrap: create setup gate: %w", err)
	}
	parentFiles = append(parentFiles, setupWriter)
	setupFD := appendExtraFile(cmd, setupReader)

	controlParent, controlChild, controlSocketInode, err := newGateSocketpair()
	if err != nil {
		return nil, fmt.Errorf("bwrap: %w", err)
	}
	closeControlParent := true
	defer func() {
		if closeControlParent {
			_ = controlParent.Close()
		}
	}()
	controlFD := appendExtraFile(cmd, controlChild)
	controlChildFD, err := strconv.Atoi(controlFD)
	if err != nil {
		return nil, fmt.Errorf("bwrap: parse native workload gate descriptor: %w", err)
	}

	argv, err := buildArgvWithLifecycle(
		opts,
		seccompFD,
		&bwrapLifecycleArgv{
			gateExecFD: gateExecFD,
			controlFD:  controlFD,
			blockFD:    setupFD,
			statusFD:   statusFD,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("bwrap: %w", err)
	}
	wrapWithArgv(cmd, bwrapPath, argv)

	lifecycle := newBwrapLifecycle(
		statusReader,
		setupWriter,
		controlParent,
		controlChildFD,
		controlSocketInode,
		!opts.ShareNet,
	)
	cleanupExtraFiles = false
	closeControlParent = false
	return lifecycle, nil
}

func appendSeccompFile(cmd *exec.Cmd) (string, error) {
	if len(seccompBPF) == 0 {
		return "", nil
	}
	fd, err := createMemfdWithData(seccompBPF)
	if err != nil {
		return "", fmt.Errorf("bwrap: seccomp memfd: %w", err)
	}
	file := os.NewFile(uintptr(fd), "seccomp")
	if file == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("bwrap: seccomp memfd: invalid descriptor")
	}
	return appendExtraFile(cmd, file), nil
}

func appendExtraFile(cmd *exec.Cmd, file *os.File) string {
	// ExtraFiles are assigned descriptors starting at 3 in the child.
	childFD := strconv.Itoa(3 + len(cmd.ExtraFiles))
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return childFD
}

func closeExtraFilesFrom(cmd *exec.Cmd, first int) {
	for _, file := range cmd.ExtraFiles[first:] {
		_ = file.Close()
	}
	cmd.ExtraFiles = cmd.ExtraFiles[:first]
}

// createMemfdWithData creates an anonymous memfd, writes data to it, and
// seeks back to the beginning. The returned fd is ready to be passed to bwrap
// via ExtraFiles.
func createMemfdWithData(data []byte) (int, error) {
	fd, err := unix.MemfdCreate("seccomp", 0)
	if err != nil {
		return -1, fmt.Errorf("memfd_create: %w", err)
	}
	// Write data and seek back to 0 so bwrap reads from the start.
	if _, err := unix.Write(fd, data); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("write seccomp BPF: %w", err)
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("seek seccomp BPF: %w", err)
	}
	return fd, nil
}

// Ensure bwrapImpl satisfies Isolator.
var _ Isolator = (*bwrapImpl)(nil)
var _ LifecycleIsolator = (*bwrapImpl)(nil)
