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

// Package streamformat registers the storage format for each record kind used
// by the built-in append-only Sinks. Registration is compile-time only.
package streamformat

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
)

// Format defines the byte encoding and object family for one RecordKind.
// Every method must be deterministic for the same inputs. An incompatible
// encoding or layout requires a new RecordKind. ObjectFamily preserves the
// legacy container-log layout; every other kind must return a family below
// <cluster>/_streams/<kind>. Sinks prepend their own configured prefix.
// ObjectMetadata contains only format-specific metadata; Sinks add and protect
// their common identity metadata separately.
type Format interface {
	Kind() api.RecordKind
	ContentType() string
	EncodeBatch(api.Batch) ([]byte, error)
	ObjectFamily(api.StreamRef, api.Resource, api.StreamMetadata) (objectlayout.Family, error)
	ObjectMetadata(api.Resource, api.StreamMetadata) (map[string]string, error)
}

var registry = struct {
	sync.RWMutex
	formats map[api.RecordKind]Format
}{formats: make(map[api.RecordKind]Format)}

// Register adds one statically linked stream format. It panics on invalid or
// duplicate registrations so binary construction cannot silently change a
// persisted object contract.
func Register(format Format) {
	if format == nil || !safePathSegment(string(format.Kind())) {
		panic("nodeagent: invalid stream format registration")
	}
	mediaType, _, err := mime.ParseMediaType(format.ContentType())
	if err != nil || mediaType == "" {
		panic("nodeagent: invalid stream format content type")
	}
	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.formats[format.Kind()]; exists {
		panic("nodeagent: duplicate stream format " + string(format.Kind()))
	}
	registry.formats[format.Kind()] = format
}

// Lookup returns the statically registered format for kind.
func Lookup(kind api.RecordKind) (Format, error) {
	registry.RLock()
	format := registry.formats[kind]
	registry.RUnlock()
	if format == nil {
		return nil, fmt.Errorf("no stream format registered for record kind %q", kind)
	}
	return format, nil
}

// Kinds returns all registered record kinds in stable order.
func Kinds() []api.RecordKind {
	registry.RLock()
	kinds := make([]api.RecordKind, 0, len(registry.formats))
	for kind := range registry.formats {
		kinds = append(kinds, kind)
	}
	registry.RUnlock()
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// EncodeBatch validates the stream-wide format invariants before delegating
// encoding to the selected format.
func EncodeBatch(batch api.Batch) (Format, api.Resource, []byte, error) {
	if len(batch.Items) == 0 {
		return nil, api.Resource{}, nil, errors.New("cannot encode an empty batch")
	}
	format, err := Lookup(batch.StreamRef.Kind)
	if err != nil {
		return nil, api.Resource{}, nil, err
	}
	if err := batch.Metadata.Validate(); err != nil {
		return nil, api.Resource{}, nil, err
	}
	resource := batch.Items[0].Record.Resource
	for _, item := range batch.Items {
		if item.Record.Kind != batch.StreamRef.Kind {
			return nil, api.Resource{}, nil, fmt.Errorf("record kind %q does not match stream kind %q", item.Record.Kind, batch.StreamRef.Kind)
		}
		if item.Record.Resource != resource {
			return nil, api.Resource{}, nil, errors.New("batch contains inconsistent resource identities")
		}
	}
	encoded, err := format.EncodeBatch(batch)
	if err != nil {
		return nil, api.Resource{}, nil, err
	}
	if len(encoded) == 0 {
		return nil, api.Resource{}, nil, errors.New("stream format encoded a non-empty batch as empty output")
	}
	return format, resource, encoded, nil
}

// ResolveFamily validates a Format-owned family against its namespace, then
// places it below the Sink-owned prefix. The existing container-log layout is
// preserved; every other kind is isolated below <cluster>/_streams/<kind>.
func ResolveFamily(format Format, prefix string, streamRef api.StreamRef, resource api.Resource, metadata api.StreamMetadata) (objectlayout.Family, error) {
	if format == nil || streamRef.Kind == "" || format.Kind() != streamRef.Kind {
		return objectlayout.Family{}, errors.New("stream format does not match stream kind")
	}
	if !safePathSegment(resource.ClusterName) || !safePathSegment(string(streamRef.Kind)) {
		return objectlayout.Family{}, errors.New("stream format has an unsafe cluster or record kind")
	}
	family, err := format.ObjectFamily(streamRef, resource, metadata)
	if err != nil {
		return objectlayout.Family{}, err
	}
	root := resource.ClusterName
	if streamRef.Kind != api.RecordKindContainerLog {
		root = path.Join(root, "_streams", string(streamRef.Kind))
	}
	if !family.Within(root) {
		return objectlayout.Family{}, fmt.Errorf("object family directory %q is outside record-kind root %q", family.Directory(), root)
	}
	return family.WithPrefix(prefix)
}

func safePathSegment(value string) bool {
	if value == "" || value == "." || value == ".." || !utf8.ValidString(value) || strings.ContainsAny(value, `/\`) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
