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

package controller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/alibaba/opensandbox/execd/pkg/util/glob"
)

const maxGuardSymlinkDepth = 40

// errForbiddenPath marks requests targeting pseudo-filesystems, internal
// platform paths, special device/FIFO nodes, or operator-denied paths/globs.
var errForbiddenPath = errors.New("access to path is forbidden")

type forbiddenPathError struct {
	path string
}

func (e *forbiddenPathError) Error() string {
	return fmt.Sprintf("access to path %q is forbidden", e.path)
}

func (e *forbiddenPathError) Unwrap() error {
	return errForbiddenPath
}

// defaultBlockedRoots are always blocked on /files/* and /directories/* APIs
// regardless of whether hardening or Landlock is enabled.
var defaultBlockedRoots = []string{
	"/proc",
	"/sys",
	"/dev",
	"/sys/fs/cgroup",
	"/var/lib/execd",
	"/etc/execd",
	"/etc/opensandbox",
	"/opt/opensandbox",
	"/var/log/opensandbox",
}

// FilesystemGuardPolicy configures additional blocked paths and glob patterns
// layered on top of defaultBlockedRoots.
type FilesystemGuardPolicy struct {
	ExtraBlockedPaths []string `json:"extra_blocked_paths,omitempty"`
	DenyGlobs         []string `json:"deny_globs,omitempty"`
}

// IsZero reports whether the policy adds no rules beyond the built-in defaults.
func (p FilesystemGuardPolicy) IsZero() bool {
	return len(p.ExtraBlockedPaths) == 0 && len(p.DenyGlobs) == 0
}

type filesystemGuard struct {
	policy       FilesystemGuardPolicy
	blockedRoots []string
	denyGlobs    []string
}

var (
	defaultFilesystemGuard = mustBuildFilesystemGuard(FilesystemGuardPolicy{})
	activeFilesystemGuard  atomic.Pointer[filesystemGuard]
)

func mustBuildFilesystemGuard(policy FilesystemGuardPolicy) *filesystemGuard {
	g, err := buildFilesystemGuard(policy)
	if err != nil {
		panic(err)
	}
	return g
}

func buildFilesystemGuard(policy FilesystemGuardPolicy) (*filesystemGuard, error) {
	seen := make(map[string]struct{}, len(defaultBlockedRoots)+len(policy.ExtraBlockedPaths))
	var roots []string
	addRoot := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		cleaned := filepath.Clean(raw)
		if !filepath.IsAbs(cleaned) {
			if abs, err := filepath.Abs(cleaned); err == nil {
				cleaned = abs
			}
		}
		if cleaned == string(filepath.Separator) || cleaned == "." {
			return
		}
		if _, ok := seen[cleaned]; !ok {
			seen[cleaned] = struct{}{}
			roots = append(roots, cleaned)
		}
		if canonical := canonicalizeExistingPrefix(cleaned); canonical != "" &&
			canonical != string(filepath.Separator) {
			if _, ok := seen[canonical]; !ok {
				seen[canonical] = struct{}{}
				roots = append(roots, canonical)
			}
		}
	}

	for _, r := range defaultBlockedRoots {
		addRoot(r)
	}
	for _, r := range policy.ExtraBlockedPaths {
		addRoot(r)
	}

	var denyGlobs []string
	for _, pattern := range policy.DenyGlobs {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !glob.ValidPattern(pattern) {
			return nil, fmt.Errorf("filesystem guard: invalid deny glob %q", pattern)
		}
		denyGlobs = append(denyGlobs, pattern)
	}

	copiedPolicy := FilesystemGuardPolicy{
		ExtraBlockedPaths: append([]string(nil), policy.ExtraBlockedPaths...),
		DenyGlobs:         append([]string(nil), denyGlobs...),
	}
	return &filesystemGuard{
		policy:       copiedPolicy,
		blockedRoots: roots,
		denyGlobs:    denyGlobs,
	}, nil
}

// canonicalizeExistingPrefix resolves symlinks on the longest existing prefix
// of cleaned so configured paths under symlinked roots (for example /var ->
// /private/var on macOS) match both their lexical and canonical forms.
func canonicalizeExistingPrefix(cleaned string) string {
	cur := cleaned
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if len(tail) == 0 {
				return filepath.Clean(resolved)
			}
			parts := make([]string, 0, len(tail)+1)
			parts = append(parts, resolved)
			for i := len(tail) - 1; i >= 0; i-- {
				parts = append(parts, tail[i])
			}
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// InitFilesystemGuard installs the process-wide filesystem path guard policy.
// Default pseudo-filesystem and platform paths are always included.
func InitFilesystemGuard(policy FilesystemGuardPolicy) error {
	g, err := buildFilesystemGuard(policy)
	if err != nil {
		return err
	}
	activeFilesystemGuard.Store(g)
	return nil
}

// CurrentFilesystemGuardPolicy returns the extra rules currently installed on
// top of defaultBlockedRoots so --filesystem-worker children inherit them.
func CurrentFilesystemGuardPolicy() FilesystemGuardPolicy {
	g := getFilesystemGuard()
	return FilesystemGuardPolicy{
		ExtraBlockedPaths: append([]string(nil), g.policy.ExtraBlockedPaths...),
		DenyGlobs:         append([]string(nil), g.policy.DenyGlobs...),
	}
}

func getFilesystemGuard() *filesystemGuard {
	if g := activeFilesystemGuard.Load(); g != nil {
		return g
	}
	return defaultFilesystemGuard
}

func checkFilesystemPath(absPath string) error {
	return getFilesystemGuard().CheckPath(absPath)
}

func isFilesystemPathBlocked(absPath string) bool {
	return getFilesystemGuard().CheckPath(absPath) != nil
}

func (g *filesystemGuard) matchesBlockedRoot(candidate string) bool {
	sep := string(filepath.Separator)
	for _, root := range g.blockedRoots {
		if candidate == root || strings.HasPrefix(candidate, root+sep) {
			return true
		}
	}
	return false
}

func (g *filesystemGuard) matchesDenyGlob(candidate string) bool {
	for _, pattern := range g.denyGlobs {
		if matched, err := glob.PathMatch(pattern, candidate); err == nil && matched {
			return true
		}
	}
	return false
}

func (g *filesystemGuard) matchesPath(candidate string) bool {
	return g.matchesBlockedRoot(candidate) || g.matchesDenyGlob(candidate)
}

// CheckPath validates absPath lexically and across every symlink hop (including
// dangling symlinks and non-existent suffixes) against blocked roots and globs.
func (g *filesystemGuard) CheckPath(absPath string) error {
	cleaned := filepath.Clean(absPath)
	if g.matchesPath(cleaned) {
		return &forbiddenPathError{path: absPath}
	}

	vol := filepath.VolumeName(cleaned)
	rest := strings.TrimPrefix(cleaned, vol)
	sep := string(filepath.Separator)
	if !strings.HasPrefix(rest, sep) {
		return nil
	}

	cur := vol + sep
	remaining := splitPathSegments(strings.TrimPrefix(rest, sep))
	hops := 0

	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		if part == "" || part == "." {
			continue
		}

		next := filepath.Join(cur, part)
		if g.matchesBlockedRoot(next) || (len(remaining) == 0 && g.matchesDenyGlob(next)) {
			return &forbiddenPathError{path: absPath}
		}

		fi, err := os.Lstat(next)
		if err != nil {
			// Entry does not exist yet or cannot be lstat'd; check the full
			// remaining lexical path and stop resolving.
			final := joinRemaining(next, remaining)
			if g.matchesPath(final) {
				return &forbiddenPathError{path: absPath}
			}
			return nil
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > maxGuardSymlinkDepth {
				// Leave symlink loops to the underlying filesystem call's ELOOP.
				return nil
			}
			target, err := os.Readlink(next)
			if err != nil {
				final := joinRemaining(next, remaining)
				if g.matchesPath(final) {
					return &forbiddenPathError{path: absPath}
				}
				return nil
			}

			var resolvedTarget string
			if filepath.IsAbs(target) {
				resolvedTarget = filepath.Clean(target)
			} else {
				// cur is already canonical (symlink-free), so Join+Clean matches
				// POSIX relative symlink resolution.
				resolvedTarget = filepath.Clean(filepath.Join(cur, target))
			}

			if g.matchesBlockedRoot(resolvedTarget) ||
				(len(remaining) == 0 && g.matchesDenyGlob(resolvedTarget)) {
				return &forbiddenPathError{path: absPath}
			}
			fullTarget := joinRemaining(resolvedTarget, remaining)
			if g.matchesPath(fullTarget) {
				return &forbiddenPathError{path: absPath}
			}

			targetVol := filepath.VolumeName(resolvedTarget)
			targetRest := strings.TrimPrefix(resolvedTarget, targetVol)
			cur = targetVol + sep
			targetSegs := splitPathSegments(strings.TrimPrefix(targetRest, sep))
			remaining = append(targetSegs, remaining...)
			continue
		}

		cur = next
	}

	if g.matchesPath(cur) {
		return &forbiddenPathError{path: absPath}
	}
	return nil
}

func splitPathSegments(rel string) []string {
	if rel == "" {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

func joinRemaining(base string, remaining []string) string {
	if len(remaining) == 0 {
		return filepath.Clean(base)
	}
	parts := make([]string, 0, len(remaining)+1)
	parts = append(parts, base)
	parts = append(parts, remaining...)
	return filepath.Clean(filepath.Join(parts...))
}
