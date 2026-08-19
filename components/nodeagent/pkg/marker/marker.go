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

package marker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

const SchemaVersion = 1

const maxSafeJSONInteger = 1<<53 - 1

const maxJSONNestingDepth = 64

type Marker struct {
	SchemaVersion     int                  `json:"schema_version"`
	TargetID          string               `json:"target_id"`
	FinalizeID        string               `json:"finalize_id"`
	Revision          uint64               `json:"revision"`
	StreamRef         string               `json:"stream_ref"`
	Resource          api.Resource         `json:"resource"`
	CoverageStartedAt string               `json:"coverage_started_at"`
	Status            string               `json:"status"`
	HadDrops          bool                 `json:"had_drops"`
	HadSourceGaps     bool                 `json:"had_source_gaps"`
	LossReasons       []string             `json:"loss_reasons"`
	FinalizedAt       string               `json:"finalized_at"`
	Objects           []state.ClosedObject `json:"objects"`
}

func New(request api.FinalizeRequest, objects []state.ClosedObject) Marker {
	reasons := append([]string(nil), request.Outcome.LossReasons...)
	sort.Strings(reasons)
	reasons = compact(reasons)
	objects = append([]state.ClosedObject(nil), objects...)
	sort.Slice(objects, func(i, j int) bool { return objects[i].Generation < objects[j].Generation })
	coverageStartedAt := ""
	if !request.CoverageStartedAt.IsZero() {
		coverageStartedAt = request.CoverageStartedAt.UTC().Format(time.RFC3339Nano)
	}
	return Marker{
		SchemaVersion:     SchemaVersion,
		TargetID:          request.TargetID,
		FinalizeID:        request.FinalizeID,
		Revision:          request.Revision,
		StreamRef:         request.StreamRef.ID,
		Resource:          request.Resource,
		CoverageStartedAt: coverageStartedAt,
		Status:            Status(request.Outcome),
		HadDrops:          request.Outcome.HadDrops,
		HadSourceGaps:     request.Outcome.HadSourceGaps,
		LossReasons:       reasons,
		FinalizedAt:       request.FinalizedAt.UTC().Truncate(time.Second).Format(time.RFC3339),
		Objects:           objects,
	}
}

func Status(outcome api.SourceOutcome) string {
	if outcome.HadSourceGaps {
		return "incomplete"
	}
	if outcome.HadDrops {
		return "complete-with-drops"
	}
	return "complete"
}

func Encode(value Marker) ([]byte, error) {
	if err := Validate(value); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1024)
	out = append(out, `{"schema_version":`...)
	out = strconv.AppendInt(out, SchemaVersion, 10)
	out = append(out, `,"target_id":`...)
	out = appendJSONString(out, value.TargetID)
	out = append(out, `,"finalize_id":`...)
	out = appendJSONString(out, value.FinalizeID)
	out = append(out, `,"revision":`...)
	out = strconv.AppendUint(out, value.Revision, 10)
	out = append(out, `,"stream_ref":`...)
	out = appendJSONString(out, value.StreamRef)
	out = append(out, `,"resource":{`...)
	out = append(out, `"sandbox_id":`...)
	out = appendJSONString(out, value.Resource.SandboxID)
	out = append(out, `,"k8s.namespace.name":`...)
	out = appendJSONString(out, value.Resource.Namespace)
	out = append(out, `,"k8s.pod.name":`...)
	out = appendJSONString(out, value.Resource.PodName)
	out = append(out, `,"k8s.pod.uid":`...)
	out = appendJSONString(out, value.Resource.PodUID)
	out = append(out, `,"k8s.container.name":`...)
	out = appendJSONString(out, value.Resource.Container)
	out = append(out, `,"k8s.node.name":`...)
	out = appendJSONString(out, value.Resource.NodeName)
	out = append(out, `,"k8s.cluster.name":`...)
	out = appendJSONString(out, value.Resource.ClusterName)
	out = append(out, `},"coverage_started_at":`...)
	out = appendJSONString(out, value.CoverageStartedAt)
	out = append(out, `,"status":`...)
	out = appendJSONString(out, value.Status)
	out = append(out, `,"had_drops":`...)
	out = strconv.AppendBool(out, value.HadDrops)
	out = append(out, `,"had_source_gaps":`...)
	out = strconv.AppendBool(out, value.HadSourceGaps)
	out = append(out, `,"loss_reasons":[`...)
	for i, reason := range value.LossReasons {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendJSONString(out, reason)
	}
	out = append(out, `],"finalized_at":`...)
	out = appendJSONString(out, value.FinalizedAt)
	out = append(out, `,"objects":[`...)
	for i, object := range value.Objects {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, `{"key":`...)
		out = appendJSONString(out, object.Key)
		out = append(out, `,"generation":`...)
		out = strconv.AppendUint(out, object.Generation, 10)
		out = append(out, `,"size":`...)
		out = strconv.AppendInt(out, object.Size, 10)
		out = append(out, `,"crc64":`...)
		out = appendJSONString(out, object.CRC64)
		out = append(out, '}')
	}
	out = append(out, ']', '}')
	return out, nil
}

func Decode(raw []byte) (Marker, error) {
	if len(raw) == 0 || !bytes.Equal(raw, bytes.TrimSpace(raw)) || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return Marker{}, errors.New("marker must be compact UTF-8 JSON without BOM or surrounding whitespace")
	}
	if !utf8.Valid(raw) {
		return Marker{}, errors.New("marker is not valid UTF-8")
	}
	if err := rejectDuplicateMembers(raw); err != nil {
		return Marker{}, err
	}
	if err := requireMembers(raw); err != nil {
		return Marker{}, err
	}
	value, err := decodeExactMembers(raw)
	if err != nil {
		return Marker{}, err
	}
	if err := Validate(value); err != nil {
		return Marker{}, err
	}
	return value, nil
}

func decodeExactMembers(raw []byte) (Marker, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Marker{}, err
	}
	var value Marker
	for _, field := range []struct {
		key         string
		destination any
	}{
		{key: "schema_version", destination: &value.SchemaVersion},
		{key: "target_id", destination: &value.TargetID},
		{key: "finalize_id", destination: &value.FinalizeID},
		{key: "revision", destination: &value.Revision},
		{key: "stream_ref", destination: &value.StreamRef},
		{key: "coverage_started_at", destination: &value.CoverageStartedAt},
		{key: "status", destination: &value.Status},
		{key: "had_drops", destination: &value.HadDrops},
		{key: "had_source_gaps", destination: &value.HadSourceGaps},
		{key: "loss_reasons", destination: &value.LossReasons},
		{key: "finalized_at", destination: &value.FinalizedAt},
	} {
		if err := json.Unmarshal(top[field.key], field.destination); err != nil {
			return Marker{}, fmt.Errorf("decode marker member %q: %w", field.key, err)
		}
	}

	var resource map[string]json.RawMessage
	if err := json.Unmarshal(top["resource"], &resource); err != nil {
		return Marker{}, fmt.Errorf("decode marker member %q: %w", "resource", err)
	}
	for _, field := range []struct {
		key         string
		destination *string
	}{
		{key: "sandbox_id", destination: &value.Resource.SandboxID},
		{key: "k8s.namespace.name", destination: &value.Resource.Namespace},
		{key: "k8s.pod.name", destination: &value.Resource.PodName},
		{key: "k8s.pod.uid", destination: &value.Resource.PodUID},
		{key: "k8s.container.name", destination: &value.Resource.Container},
		{key: "k8s.node.name", destination: &value.Resource.NodeName},
		{key: "k8s.cluster.name", destination: &value.Resource.ClusterName},
	} {
		if err := json.Unmarshal(resource[field.key], field.destination); err != nil {
			return Marker{}, fmt.Errorf("decode resource member %q: %w", field.key, err)
		}
	}

	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(top["objects"], &objects); err != nil {
		return Marker{}, fmt.Errorf("decode marker member %q: %w", "objects", err)
	}
	value.Objects = make([]state.ClosedObject, len(objects))
	for i, object := range objects {
		for _, field := range []struct {
			key         string
			destination any
		}{
			{key: "key", destination: &value.Objects[i].Key},
			{key: "generation", destination: &value.Objects[i].Generation},
			{key: "size", destination: &value.Objects[i].Size},
			{key: "crc64", destination: &value.Objects[i].CRC64},
		} {
			if err := json.Unmarshal(object[field.key], field.destination); err != nil {
				return Marker{}, fmt.Errorf("decode object %d member %q: %w", i, field.key, err)
			}
		}
	}
	return value, nil
}

func Validate(value Marker) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported marker schema %d", value.SchemaVersion)
	}
	if value.TargetID == "" || value.FinalizeID == "" || value.StreamRef == "" || value.Revision == 0 {
		return errors.New("marker identity fields are required")
	}
	if value.Revision > maxSafeJSONInteger {
		return errors.New("marker revision exceeds the safe JSON integer range")
	}
	resource := value.Resource
	if resource.SandboxID == "" || resource.ClusterName == "" || resource.Namespace == "" || resource.PodName == "" || resource.PodUID == "" || resource.Container == "" || resource.NodeName == "" {
		return errors.New("marker resource fields are required")
	}
	for _, text := range []string{value.TargetID, value.FinalizeID, value.StreamRef, resource.SandboxID, resource.ClusterName, resource.Namespace, resource.PodName, resource.PodUID, resource.Container, resource.NodeName, value.CoverageStartedAt, value.Status, value.FinalizedAt} {
		if !utf8.ValidString(text) {
			return errors.New("marker contains invalid UTF-8")
		}
	}
	if value.Status != Status(api.SourceOutcome{HadDrops: value.HadDrops, HadSourceGaps: value.HadSourceGaps}) {
		return errors.New("marker status does not match outcome")
	}
	if (value.HadDrops || value.HadSourceGaps) != (len(value.LossReasons) > 0) {
		return errors.New("marker loss flags do not match loss reasons")
	}
	coverageStartedAt, err := time.Parse(time.RFC3339, value.CoverageStartedAt)
	if err != nil {
		return fmt.Errorf("invalid coverage_started_at: %w", err)
	}
	if coverageStartedAt.Location() != time.UTC || coverageStartedAt.Nanosecond() != 0 || coverageStartedAt.Format(time.RFC3339) != value.CoverageStartedAt {
		return errors.New("coverage_started_at must be canonical UTC RFC3339 at second precision")
	}
	finalizedAt, err := time.Parse(time.RFC3339, value.FinalizedAt)
	if err != nil {
		return fmt.Errorf("invalid finalized_at: %w", err)
	}
	if finalizedAt.Location() != time.UTC || finalizedAt.Nanosecond() != 0 || finalizedAt.Format(time.RFC3339) != value.FinalizedAt {
		return errors.New("finalized_at must be canonical UTC RFC3339 at second precision")
	}
	if finalizedAt.Before(coverageStartedAt) {
		return errors.New("finalized_at must not precede coverage_started_at")
	}
	for _, reason := range value.LossReasons {
		if reason == "" || !utf8.ValidString(reason) {
			return errors.New("marker loss reason is invalid")
		}
	}
	if !sort.StringsAreSorted(value.LossReasons) {
		return errors.New("marker loss reasons must be sorted")
	}
	for i := 1; i < len(value.LossReasons); i++ {
		if value.LossReasons[i] == value.LossReasons[i-1] {
			return errors.New("marker loss reasons must be unique")
		}
	}
	for i, object := range value.Objects {
		if object.Generation != uint64(i) {
			return errors.New("marker object generations must be continuous from zero")
		}
		if object.Key == "" || object.Size < 0 || object.Size > maxSafeJSONInteger || object.Generation > maxSafeJSONInteger || !canonicalDecimal(object.CRC64) {
			return errors.New("marker object fields are invalid")
		}
		if !utf8.ValidString(object.Key) {
			return errors.New("marker object key is not valid UTF-8")
		}
	}
	return nil
}

func appendJSONString(out []byte, value string) []byte {
	out = append(out, '"')
	for _, char := range value {
		switch char {
		case '"', '\\':
			out = append(out, '\\', byte(char))
		case '\b':
			out = append(out, `\b`...)
		case '\f':
			out = append(out, `\f`...)
		case '\n':
			out = append(out, `\n`...)
		case '\r':
			out = append(out, `\r`...)
		case '\t':
			out = append(out, `\t`...)
		default:
			if char < 0x20 {
				out = append(out, `\u00`...)
				const hex = "0123456789abcdef"
				out = append(out, hex[byte(char)>>4], hex[byte(char)&0x0f])
			} else {
				out = utf8.AppendRune(out, char)
			}
		}
	}
	return append(out, '"')
}

func canonicalDecimal(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func rejectDuplicateMembers(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := parseJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err == nil {
		return fmt.Errorf("trailing JSON token %v", token)
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func parseJSONValue(decoder *json.Decoder) error {
	return parseJSONValueAtDepth(decoder, 0)
}

func parseJSONValueAtDepth(decoder *json.Decoder, depth int) error {
	if depth > maxJSONNestingDepth {
		return errors.New("marker JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object member name is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			if err := parseJSONValueAtDepth(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := parseJSONValueAtDepth(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireMembers(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	for _, key := range []string{"schema_version", "target_id", "finalize_id", "revision", "stream_ref", "resource", "coverage_started_at", "status", "had_drops", "had_source_gaps", "loss_reasons", "finalized_at", "objects"} {
		value, exists := top[key]
		if !exists {
			return fmt.Errorf("missing required marker member %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("marker member %q must not be null", key)
		}
	}
	var resource map[string]json.RawMessage
	if err := json.Unmarshal(top["resource"], &resource); err != nil {
		return err
	}
	for _, key := range []string{"sandbox_id", "k8s.cluster.name", "k8s.namespace.name", "k8s.pod.name", "k8s.pod.uid", "k8s.container.name", "k8s.node.name"} {
		value, exists := resource[key]
		if !exists {
			return fmt.Errorf("missing required resource member %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("resource member %q must not be null", key)
		}
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(top["objects"], &objects); err != nil {
		return err
	}
	for _, object := range objects {
		for _, key := range []string{"key", "generation", "size", "crc64"} {
			value, exists := object[key]
			if !exists {
				return fmt.Errorf("missing required object member %q", key)
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("object member %q must not be null", key)
			}
		}
	}
	return nil
}

func compact(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
