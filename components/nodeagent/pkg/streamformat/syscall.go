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

package streamformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
)

type syscallFormat struct{}

func init() { Register(syscallFormat{}) }

func SyscallStreamID(podUID, container string) string {
	return path.Join(api.SourceNameSyscalls, podUID, container)
}

func (syscallFormat) Kind() api.RecordKind { return api.RecordKindSyscall }

func (syscallFormat) ContentType() string { return "application/x-ndjson" }

func (syscallFormat) EncodeBatch(batch api.Batch) ([]byte, error) {
	var out bytes.Buffer
	for _, item := range batch.Items {
		body := item.Record.Body
		if len(body) == 0 || bytes.IndexByte(body, '\n') >= 0 || !json.Valid(body) || body[0] != '{' {
			return nil, errors.New("syscall record body must be one JSON object without a newline")
		}
		out.Write(body)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func (syscallFormat) ObjectFamily(streamRef api.StreamRef, resource api.Resource, _ api.StreamMetadata) (objectlayout.Family, error) {
	if streamRef.ID != SyscallStreamID(resource.PodUID, resource.Container) {
		return objectlayout.Family{}, errors.New("syscall stream reference does not match its resource identity")
	}
	return objectlayout.NewFamily("", []string{resource.ClusterName, "_streams", string(api.RecordKindSyscall), resource.Namespace, resource.SandboxID, resource.PodUID}, resource.Container+".syscalls", ".jsonl")
}

func (syscallFormat) ObjectMetadata(_ api.Resource, _ api.StreamMetadata) (map[string]string, error) {
	return nil, nil
}
