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

// Package objectlayout defines backend-independent names for stream data
// objects and finalization markers.
package objectlayout

import (
	"errors"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
)

const markerInfix = ".finalized."

// Family describes one stream's backend-independent object family. Directory
// is relative to a Sink root or object prefix; Base and DataExtension are safe
// path-leaf components.
type Family struct {
	directory     string
	base          string
	dataExtension string
}

// NewFamily constructs a family without allowing a caller-provided segment to
// change directory boundaries.
func NewFamily(prefix string, directorySegments []string, base, dataExtension string) (Family, error) {
	if err := validateRelativeDirectory(prefix); err != nil {
		return Family{}, err
	}
	if len(directorySegments) == 0 {
		return Family{}, errors.New("object family directory must contain at least one segment")
	}
	for _, segment := range directorySegments {
		if !SafeSegment(segment) {
			return Family{}, errors.New("object family directory contains an unsafe path segment")
		}
	}
	if !SafeSegment(base) {
		return Family{}, errors.New("object family base must be a safe path segment")
	}
	if !validDataExtension(dataExtension) {
		return Family{}, errors.New("object family data extension must be a safe dot-prefixed suffix")
	}
	parts := make([]string, 0, len(directorySegments)+1)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, directorySegments...)
	return Family{directory: path.Join(parts...), base: base, dataExtension: dataExtension}, nil
}

// Directory returns the family's relative directory path.
func (f Family) Directory() string { return f.directory }

// WithPrefix returns the same family below a Sink-owned relative prefix.
func (f Family) WithPrefix(prefix string) (Family, error) {
	if err := validateRelativeDirectory(prefix); err != nil {
		return Family{}, err
	}
	if prefix == "" {
		return f, nil
	}
	f.directory = path.Join(prefix, f.directory)
	return f, nil
}

// Within reports whether the family directory is the given relative root or
// one of its descendants.
func (f Family) Within(root string) bool {
	if validateRelativeDirectory(root) != nil || root == "" {
		return false
	}
	return f.directory == root || strings.HasPrefix(f.directory, root+"/")
}

func (f Family) generationName(generation uint64) string {
	if generation == 0 {
		return f.base + f.dataExtension
	}
	return f.base + "." + strconv.FormatUint(generation, 10) + f.dataExtension
}

// DataKey returns the family-relative object key holding generation's data.
func (f Family) DataKey(generation uint64) string {
	return path.Join(f.directory, f.generationName(generation))
}

// MarkerName returns the family-relative marker file name for a revision.
func (f Family) MarkerName(revision uint64) string {
	return f.base + markerInfix + strconv.FormatUint(revision, 10) + ".json"
}

// MarkerKey returns the family-relative object key holding a revision's
// finalization marker.
func (f Family) MarkerKey(revision uint64) string {
	return path.Join(f.directory, f.MarkerName(revision))
}

// The helpers below preserve the original container-log object layout used by
// the offline cleanup command and existing persisted objects. They must stay
// aligned with the Family methods, which generalize the same scheme.
func generationName(container string, generation uint64) string {
	if generation == 0 {
		return container + ".log"
	}
	return container + "." + strconv.FormatUint(generation, 10) + ".log"
}

// DataKey returns the legacy container-log data object key.
func DataKey(familyPrefix, container string, generation uint64) string {
	return path.Join(familyPrefix, generationName(container, generation))
}

// MarkerPrefix returns the legacy marker key prefix for one container family.
func MarkerPrefix(familyPrefix, container string) string {
	return path.Join(familyPrefix, container) + markerInfix
}

func markerName(container string, revision uint64) string {
	return container + markerInfix + strconv.FormatUint(revision, 10) + ".json"
}

// MarkerKey returns the legacy finalization marker key for a revision.
func MarkerKey(familyPrefix, container string, revision uint64) string {
	return path.Join(familyPrefix, markerName(container, revision))
}

// StreamRef returns the legacy container-log stream identity below the
// container-logs Source namespace.
func StreamRef(podUID, container string) string {
	return path.Join(api.SourceNameContainerLogs, podUID, container)
}

func validateRelativeDirectory(directory string) error {
	if directory == "" {
		return nil
	}
	if !utf8.ValidString(directory) || strings.HasPrefix(directory, "/") || strings.Contains(directory, `\`) || path.Clean(directory) != directory {
		return errors.New("object family directory must be a clean relative path")
	}
	for _, segment := range strings.Split(directory, "/") {
		if !SafeSegment(segment) {
			return errors.New("object family directory contains an unsafe path segment")
		}
	}
	return nil
}

// SafeSegment reports whether value can be used as one path-leaf component:
// non-empty, not "." or "..", free of path separators and control characters,
// and valid UTF-8.
func SafeSegment(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validDataExtension accepts any safe dot-prefixed suffix except one shaped
// like a canonical finalization marker name (.finalized.<canonical-uint>.json);
// such a name would collide with MarkerName outputs.
func validDataExtension(value string) bool {
	if len(value) < 2 || value[0] != '.' || !SafeSegment(value[1:]) {
		return false
	}
	if !strings.HasPrefix(value, markerInfix) || !strings.HasSuffix(value, ".json") {
		return true
	}
	revision := strings.TrimSuffix(strings.TrimPrefix(value, markerInfix), ".json")
	if revision == "" || len(revision) > 1 && revision[0] == '0' {
		return true
	}
	_, err := strconv.ParseUint(revision, 10, 64)
	return err != nil
}
