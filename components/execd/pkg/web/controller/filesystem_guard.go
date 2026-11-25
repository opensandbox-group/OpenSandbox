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
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	windowsMode  bool
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
	return buildFilesystemGuardForOS(policy, runtime.GOOS == "windows")
}

func buildFilesystemGuardForOS(policy FilesystemGuardPolicy, windowsMode bool) (*filesystemGuard, error) {
	seen := make(map[string]struct{}, len(defaultBlockedRoots)+len(policy.ExtraBlockedPaths))
	var roots []string
	var normalizedExtras []string
	extraSeen := make(map[string]struct{}, len(policy.ExtraBlockedPaths))

	addRoot := func(raw string, recordExtra bool) {
		cleaned, ok := normalizeBlockedRootInput(raw, windowsMode)
		if !ok {
			return
		}
		if recordExtra {
			if _, exists := extraSeen[cleaned]; !exists {
				extraSeen[cleaned] = struct{}{}
				normalizedExtras = append(normalizedExtras, cleaned)
			}
		}
		appendNormalizedRoot(&roots, seen, cleaned, windowsMode)
		if !windowsMode || runtime.GOOS == "windows" {
			if canonical := canonicalizeExistingPrefix(cleaned); canonical != "" &&
				canonical != filepath.Dir(canonical) {
				appendNormalizedRoot(&roots, seen, canonical, windowsMode)
			}
		}
	}

	for _, r := range defaultBlockedRoots {
		addRoot(r, false)
	}
	for _, r := range policy.ExtraBlockedPaths {
		addRoot(r, true)
	}

	denyGlobs, err := normalizeDenyGlobs(policy.DenyGlobs, windowsMode)
	if err != nil {
		return nil, err
	}

	copiedPolicy := FilesystemGuardPolicy{
		ExtraBlockedPaths: normalizedExtras,
		DenyGlobs:         append([]string(nil), denyGlobs...),
	}
	return &filesystemGuard{
		policy:       copiedPolicy,
		blockedRoots: roots,
		denyGlobs:    denyGlobs,
		windowsMode:  windowsMode,
	}, nil
}

func normalizeBlockedRootInput(raw string, windowsMode bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if windowsMode && isWindowsAbsPath(raw) {
		trimmed := strings.TrimRight(strings.ReplaceAll(raw, "\\", "/"), "/")
		if len(trimmed) <= 2 { // e.g. "C:"
			return "", false
		}
		return raw, true
	}
	cleaned := filepath.Clean(raw)
	if !filepath.IsAbs(cleaned) {
		if abs, err := filepath.Abs(cleaned); err == nil {
			cleaned = abs
		}
	}
	if cleaned == "." || cleaned == filepath.Dir(cleaned) {
		return "", false
	}
	return cleaned, true
}

func isWindowsAbsPath(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		c := p[0]
		return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	return strings.HasPrefix(p, `\\`)
}

func appendNormalizedRoot(roots *[]string, seen map[string]struct{}, path string, windowsMode bool) {
	key := normalizeForMatch(path, windowsMode)
	if key == "" || key == "/" {
		return
	}
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*roots = append(*roots, key)
}

func normalizeDenyGlobs(patterns []string, windowsMode bool) ([]string, error) {
	var out []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		normalized := normalizeGlobPattern(pattern, windowsMode)
		if !glob.ValidPattern(normalized) {
			return nil, fmt.Errorf("filesystem guard: invalid deny glob %q", pattern)
		}
		out = append(out, normalized)
	}
	return out, nil
}

// normalizeForMatch converts paths into a canonical slash-separated form for
// prefix and glob comparisons, folding case on Windows.
func normalizeForMatch(p string, windowsMode bool) string {
	s := strings.ReplaceAll(p, "\\", "/")
	if !windowsMode {
		s = filepath.ToSlash(p)
	} else {
		s = strings.ToLower(s)
	}
	return strings.TrimRight(s, "/")
}

func normalizeGlobPattern(pattern string, windowsMode bool) string {
	if !windowsMode {
		return pattern
	}
	return strings.ToLower(strings.ReplaceAll(pattern, "\\", "/"))
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
// top of defaultBlockedRoots (with absolute normalized paths) so
// --filesystem-worker children inherit them identically regardless of cwd.
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

// isWalkEntryBlocked checks a directory-walk entry without performing an
// extra Lstat walk for ordinary non-symlink files. Symlinks are resolved
// fully, and directories are checked for blocked pseudo-filesystem mounts.
func isWalkEntryBlocked(entryPath string, entry fs.DirEntry) bool {
	g := getFilesystemGuard()
	cleaned := filepath.Clean(entryPath)
	if g.matchesPath(cleaned) {
		return true
	}
	if entry == nil {
		return g.CheckPath(cleaned) != nil
	}
	if entry.Type()&fs.ModeSymlink != 0 {
		return g.CheckPath(cleaned) != nil
	}
	if entry.IsDir() {
		return isBlockedDirFS(cleaned)
	}
	return false
}

func (g *filesystemGuard) matchesBlockedRoot(candidate string) bool {
	norm := normalizeForMatch(candidate, g.windowsMode)
	for _, root := range g.blockedRoots {
		if norm == root || strings.HasPrefix(norm, root+"/") {
			return true
		}
	}
	return false
}

func (g *filesystemGuard) matchesDenyGlob(candidate string) bool {
	if len(g.denyGlobs) == 0 {
		return false
	}
	norm := normalizeForMatch(candidate, g.windowsMode)
	for _, pattern := range g.denyGlobs {
		if matchGuardGlob(pattern, norm, g.windowsMode) {
			return true
		}
	}
	return false
}

func matchGuardGlob(pattern, normalizedPath string, windowsMode bool) bool {
	if windowsMode {
		// On Windows filepath.Separator is '\', whereas normalizedPath uses '/'.
		// Convert both to filepath.Separator for glob.PathMatch.
		sep := string(filepath.Separator)
		p := strings.ReplaceAll(pattern, "/", sep)
		c := strings.ReplaceAll(normalizedPath, "/", sep)
		matched, err := glob.PathMatch(p, c)
		return err == nil && matched
	}
	matched, err := glob.PathMatch(pattern, normalizedPath)
	return err == nil && matched
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

	cur, remaining, ok := splitAbsolutePath(cleaned)
	if !ok {
		return nil
	}

	hops := 0
	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		if part == "" || part == "." {
			continue
		}

		next := filepath.Join(cur, part)
		if g.isStepBlocked(next, len(remaining) == 0) {
			return &forbiddenPathError{path: absPath}
		}

		fi, err := os.Lstat(next)
		if err != nil {
			if g.matchesPath(joinRemaining(next, remaining)) {
				return &forbiddenPathError{path: absPath}
			}
			return nil
		}

		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}

		hops++
		if hops > maxGuardSymlinkDepth {
			return &forbiddenPathError{path: absPath}
		}

		nextCur, nextRem, done, err := g.resolveSymlinkStep(absPath, cur, next, remaining)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		cur, remaining = nextCur, nextRem
	}

	if g.matchesPath(cur) {
		return &forbiddenPathError{path: absPath}
	}
	return nil
}

func (g *filesystemGuard) isStepBlocked(path string, isFinal bool) bool {
	return g.matchesBlockedRoot(path) || (isFinal && g.matchesDenyGlob(path))
}

func (g *filesystemGuard) resolveSymlinkStep(
	origPath, curDir, linkPath string,
	remaining []string,
) (string, []string, bool, error) {
	target, err := os.Readlink(linkPath)
	if err != nil {
		if g.matchesPath(joinRemaining(linkPath, remaining)) {
			return "", nil, false, &forbiddenPathError{path: origPath}
		}
		return "", nil, true, nil
	}

	resolvedTarget := resolveSymlinkTarget(curDir, target)
	if g.isStepBlocked(resolvedTarget, len(remaining) == 0) ||
		g.matchesPath(joinRemaining(resolvedTarget, remaining)) {
		return "", nil, false, &forbiddenPathError{path: origPath}
	}

	nextCur, targetSegs, ok := splitAbsolutePath(resolvedTarget)
	if !ok {
		return "", nil, true, nil
	}
	return nextCur, append(targetSegs, remaining...), false, nil
}

func resolveSymlinkTarget(curDir, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(curDir, target))
}

func splitAbsolutePath(cleaned string) (string, []string, bool) {
	vol := filepath.VolumeName(cleaned)
	rest := strings.TrimPrefix(cleaned, vol)
	sep := string(filepath.Separator)
	if !strings.HasPrefix(rest, sep) {
		return "", nil, false
	}
	rel := strings.TrimPrefix(rest, sep)
	if rel == "" {
		return vol + sep, nil, true
	}
	return vol + sep, strings.Split(rel, sep), true
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
