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

// Package cgroupfs provides the cgroup filesystem primitives shared by
// execd components: /proc/self/cgroup parsing, hierarchy path resolution,
// and cgroup mount discovery. One shared implementation keeps the resource
// metrics and the OOM watcher from drifting apart on which cgroup scopes
// the sandbox.
package cgroupfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Test-overridable input path.
var mountsFile = "/proc/self/mounts"

// Entry is one line of /proc/self/cgroup.
type Entry struct {
	Controllers []string // v1 controller names; empty for the "0::" v2 line
	Path        string
}

// ParseEntries parses /proc/self/cgroup lines of the form
// "hierarchy:controllers:path". Malformed lines are skipped.
func ParseEntries(data []byte) []Entry {
	var entries []Entry
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		entry := Entry{Path: parts[2]}
		if parts[1] != "" {
			for _, name := range strings.Split(parts[1], ",") {
				if name != "" {
					entry.Controllers = append(entry.Controllers, name)
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// V2RelPath returns the sandbox's unified-hierarchy path. Hybrid hosts (v1
// controllers alongside a unified hierarchy) return "": there the "0::"
// entry is degenerate — memory accounting lives on the v1 hierarchy, and
// reading the unified path would attribute the wrong scope.
func V2RelPath(entries []Entry) string {
	for _, entry := range entries {
		if slices.Contains(entry.Controllers, "memory") {
			return ""
		}
	}
	for _, entry := range entries {
		if len(entry.Controllers) == 0 {
			return entry.Path
		}
	}
	return ""
}

// V1RelPath returns the v1 hierarchy path of the first entry whose
// controller list contains any of the given names.
func V1RelPath(entries []Entry, names ...string) string {
	for _, entry := range entries {
		if len(entry.Controllers) == 0 {
			continue
		}
		for _, name := range names {
			if slices.Contains(entry.Controllers, name) {
				return entry.Path
			}
		}
	}
	return ""
}

// Dir resolves a /proc/self/cgroup path to its directory under the
// controller hierarchy mounted at mountPoint. A cgroup v1 mount can expose a
// subtree instead of the hierarchy root (Docker on v1 without a cgroup
// namespace mounts the container's own cgroup, e.g. root "/docker/<id>"),
// and the process path then already names the mount's top directory. The
// mount root comes from the /proc/self/mountinfo data; when the mount is
// absent there, or the path lies outside its root, the path is joined to
// the mount point unchanged.
func Dir(mountinfo []byte, mountPoint, cgroupPath string) string {
	if root, ok := mountRoot(mountinfo, mountPoint); ok && root != "/" {
		cgroupPath = cleanSlash(cgroupPath)
		if cgroupPath == root {
			return mountPoint
		}
		if rel, found := strings.CutPrefix(cgroupPath, root+"/"); found {
			return filepath.Join(mountPoint, filepath.FromSlash(rel))
		}
	}
	return filepath.Join(mountPoint, filepath.FromSlash(cgroupPath))
}

// mountRoot returns the root field of the mountinfo entry mounted at
// mountPoint. The last matching entry wins: it is the mount stacked on top.
func mountRoot(mountinfo []byte, mountPoint string) (string, bool) {
	targets := []string{filepath.Clean(mountPoint)}
	if resolved, err := filepath.EvalSymlinks(mountPoint); err == nil && resolved != targets[0] {
		targets = append(targets, resolved)
	}
	var root string
	var found bool
	for _, line := range strings.Split(string(mountinfo), "\n") {
		// "id parent major:minor root mountpoint options ..."
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if slices.Contains(targets, unescapeMountField(fields[4])) {
			root, found = cleanSlash(unescapeMountField(fields[3])), true
		}
	}
	return root, found
}

// cleanSlash cleans a slash-separated cgroup or mount root path.
func cleanSlash(p string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
}

// unescapeMountField decodes the octal escapes (\040 for space, \134 for
// backslash, ...) the kernel uses for paths in mount tables.
func unescapeMountField(field string) string {
	if !strings.Contains(field, "\\") {
		return field
	}
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+4 <= len(field) {
			if v, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

// ScanMounts reads /proc/self/mounts and returns the mount point of the
// first entry the matcher accepts.
func ScanMounts(match func(mountPoint, fstype string, options []string) bool) (string, error) {
	data, err := os.ReadFile(mountsFile)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if match(fields[1], fields[2], fields[3:]) {
			return fields[1], nil
		}
	}
	return "", errors.New("no matching mount in " + mountsFile)
}

// OptionsHave reports whether the mount options contain the given option.
// /proc/self/mounts packs options into one comma-separated field, e.g.
// "rw,nosuid,nodev,memory"; option prefixes do not match.
func OptionsHave(options []string, want string) bool {
	for _, field := range options {
		for _, opt := range strings.Split(field, ",") {
			if opt == want {
				return true
			}
		}
	}
	return false
}
