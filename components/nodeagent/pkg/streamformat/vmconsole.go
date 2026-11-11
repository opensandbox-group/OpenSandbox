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
	"errors"
	"path"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
)

type vmConsoleFormat struct{}

func init() { Register(vmConsoleFormat{}) }

func (vmConsoleFormat) Kind() api.RecordKind { return api.RecordKindVMConsole }
func (vmConsoleFormat) ContentType() string  { return "application/octet-stream" }
func (vmConsoleFormat) EncodeBatch(batch api.Batch) ([]byte, error) {
	var out bytes.Buffer
	for _, item := range batch.Items {
		out.Write(item.Record.Body)
	}
	return out.Bytes(), nil
}

func (vmConsoleFormat) ObjectFamily(ref api.StreamRef, resource api.Resource, metadata api.StreamMetadata) (objectlayout.Family, error) {
	uid := metadata["sandbox-uid"]
	if uid == "" || ref.ID != path.Join(api.SourceNameVMConsole, uid) {
		return objectlayout.Family{}, errors.New("VM console stream does not match its sandbox UID")
	}
	return objectlayout.NewFamily("", []string{resource.ClusterName, "_streams", string(api.RecordKindVMConsole), resource.Namespace, resource.SandboxID, uid}, "console", ".log")
}

func (vmConsoleFormat) ObjectMetadata(_ api.Resource, metadata api.StreamMetadata) (map[string]string, error) {
	return metadata.Clone(), nil
}
