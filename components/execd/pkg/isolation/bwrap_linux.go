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
	"os/exec"
)

// bwrapPath is the path to the bwrap binary. It is discovered at startup by
// findBwrap and cached for subsequent use.
var bwrapPath string

// findBwrap locates the bwrap binary. Priority order:
//
//  1. $PATH lookup                 — respect user-installed bwrap
//  2. /opt/opensandbox/bin/bwrap   — injected by init container alongside execd
//  3. /usr/bin/bwrap               — system package (Alpine apk)
//  4. /usr/local/bin/bwrap         — manual install
func findBwrap() string {
	// First: respect whatever the user has in $PATH.
	if path, err := exec.LookPath("bwrap"); err == nil {
		return path
	}
	// Fall back to known locations.
	for _, p := range []string{
		"/opt/opensandbox/bin/bwrap",
		"/usr/bin/bwrap",
		"/usr/local/bin/bwrap",
	} {
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	return ""
}

// bwrapImpl is the Linux bwrap Isolator.
type bwrapImpl struct {
	seccompPath string
}

// NewBwrap returns a bwrap Isolator for Linux.
func NewBwrap() Isolator {
	bwrapPath = findBwrap()
	return &bwrapImpl{}
}

func (b *bwrapImpl) Name() string { return "bwrap" }

func (b *bwrapImpl) Available() bool {
	if bwrapPath == "" {
		bwrapPath = findBwrap()
	}
	return bwrapPath != ""
}

func (b *bwrapImpl) Capabilities() Capabilities {
	if bwrapPath == "" {
		bwrapPath = findBwrap()
	}

	version, err := probeBwrapVersion()
	if err != nil {
		version = ""
	}

	return Capabilities{
		Available:              bwrapPath != "",
		Isolator:               "bwrap",
		Version:                version,
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

	seccompPath := b.seccompPath
	argv, err := buildArgv(opts, seccompPath)
	if err != nil {
		return fmt.Errorf("bwrap: %w", err)
	}

	wrapWithArgv(cmd, bwrapPath, argv)
	return nil
}

// SetSeccompPath sets the path to a seccomp BPF profile for this bwrap
// instance.
func (b *bwrapImpl) SetSeccompPath(path string) {
	b.seccompPath = path
}

// Ensure bwrapImpl satisfies Isolator.
var _ Isolator = (*bwrapImpl)(nil)
