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

package streamformat

import (
	"bytes"
	"errors"
	"path/filepath"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
)

type containerLogFormat struct{}

const ContainerLogDirectoryMetadata = "log-directory"

func init() { Register(containerLogFormat{}) }

// ContainerLogStreamID returns the stable Source-owned identity used by the
// built-in container-logs Source and its legacy recovery state.
func ContainerLogStreamID(podUID, container string) string {
	return objectlayout.StreamRef(podUID, container)
}

func (containerLogFormat) Kind() api.RecordKind { return api.RecordKindContainerLog }

func (containerLogFormat) ContentType() string { return "application/octet-stream" }

func (containerLogFormat) EncodeBatch(batch api.Batch) ([]byte, error) {
	var out bytes.Buffer
	for _, item := range batch.Items {
		stream := item.Record.Attributes["stream"]
		if stream != "stdout" && stream != "stderr" {
			return nil, errors.New("container-log record stream must be stdout or stderr")
		}
		if bytes.IndexByte(item.Record.Body, '\n') >= 0 {
			return nil, errors.New("container-log record body contains a newline")
		}
		if item.Record.Timestamp.IsZero() {
			return nil, errors.New("container-log record timestamp is required")
		}
		out.WriteString(item.Record.Timestamp.UTC().Format(time.RFC3339Nano))
		out.WriteByte(' ')
		out.WriteString(stream)
		out.WriteByte(' ')
		out.Write(item.Record.Body)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func (containerLogFormat) ObjectFamily(streamRef api.StreamRef, resource api.Resource, metadata api.StreamMetadata) (objectlayout.Family, error) {
	if _, err := containerLogDirectory(metadata); err != nil {
		return objectlayout.Family{}, err
	}
	if streamRef.ID != ContainerLogStreamID(resource.PodUID, resource.Container) {
		return objectlayout.Family{}, errors.New("container-log stream reference does not match its resource identity")
	}
	return objectlayout.NewFamily("", []string{resource.ClusterName, resource.Namespace, resource.SandboxID, resource.PodUID}, resource.Container, ".log")
}

func (containerLogFormat) ObjectMetadata(_ api.Resource, metadata api.StreamMetadata) (map[string]string, error) {
	directory, err := containerLogDirectory(metadata)
	if err != nil {
		return nil, err
	}
	return map[string]string{ContainerLogDirectoryMetadata: directory}, nil
}

func containerLogDirectory(metadata api.StreamMetadata) (string, error) {
	directory := metadata[ContainerLogDirectoryMetadata]
	if directory == "" || !filepath.IsAbs(directory) {
		return "", errors.New("container-log format requires an absolute log directory in stream metadata")
	}
	return directory, nil
}
