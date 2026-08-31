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
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/objectlayout"
)

func TestContainerLogFormatPreservesWireContract(t *testing.T) {
	resource := api.Resource{ClusterName: "prod", Namespace: "ns", SandboxID: "sb", PodUID: "uid", Container: "sandbox"}
	streamMetadata := api.StreamMetadata{ContainerLogDirectoryMetadata: "/var/log/pods/ns_pod_uid/sandbox"}
	batch := api.Batch{
		StreamRef: api.StreamRef{ID: ContainerLogStreamID("uid", "sandbox"), Kind: api.RecordKindContainerLog},
		Metadata:  streamMetadata,
		Items:     []api.BatchItem{{Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC), Body: []byte("hello"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}},
	}
	format, gotResource, encoded, err := EncodeBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	if format.ContentType() != "application/octet-stream" || gotResource != resource || string(encoded) != "2026-07-23T10:00:00Z stdout hello\n" {
		t.Fatalf("format=%q resource=%+v encoded=%q", format.ContentType(), gotResource, encoded)
	}
	if batch.StreamRef.ID != "container-logs/uid/sandbox" {
		t.Fatalf("stream ref=%q", batch.StreamRef.ID)
	}
	family, err := ResolveFamily(format, "logs", batch.StreamRef, resource, streamMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if family.DataKey(0) != "logs/prod/ns/sb/uid/sandbox.log" || family.DataKey(2) != "logs/prod/ns/sb/uid/sandbox.2.log" || family.MarkerKey(3) != "logs/prod/ns/sb/uid/sandbox.finalized.3.json" {
		t.Fatalf("unexpected family: data0=%q data2=%q marker=%q", family.DataKey(0), family.DataKey(2), family.MarkerKey(3))
	}
	metadata, err := format.ObjectMetadata(resource, streamMetadata)
	if err != nil || metadata["log-directory"] != streamMetadata[ContainerLogDirectoryMetadata] {
		t.Fatalf("metadata=%v err=%v", metadata, err)
	}
}

func TestContainerLogFormatRejectsInvalidFraming(t *testing.T) {
	resource := api.Resource{ClusterName: "prod", Namespace: "ns", SandboxID: "sb", PodUID: "uid", Container: "sandbox"}
	for _, test := range []struct {
		name   string
		stream string
		body   string
		stamp  time.Time
	}{
		{name: "invalid stream", stream: "unknown", body: "hello", stamp: time.Now()},
		{name: "embedded newline", stream: "stdout", body: "hello\nworld", stamp: time.Now()},
		{name: "missing timestamp", stream: "stdout", body: "hello"},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch := api.Batch{
				StreamRef: api.StreamRef{ID: ContainerLogStreamID("uid", "sandbox"), Kind: api.RecordKindContainerLog},
				Items: []api.BatchItem{{Record: api.Record{
					Kind:       api.RecordKindContainerLog,
					Timestamp:  test.stamp,
					Body:       []byte(test.body),
					Resource:   resource,
					Attributes: map[string]string{"stream": test.stream},
				}}},
			}
			if _, _, _, err := EncodeBatch(batch); err == nil {
				t.Fatal("EncodeBatch() accepted invalid container-log framing")
			}
		})
	}
}

func TestContainerLogFormatRejectsMismatchedStreamIdentity(t *testing.T) {
	format, err := Lookup(api.RecordKindContainerLog)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{ClusterName: "prod", Namespace: "ns", SandboxID: "sb", PodUID: "uid", Container: "sandbox"}
	if _, err := ResolveFamily(format, "", api.StreamRef{ID: ContainerLogStreamID("other", "sandbox"), Kind: api.RecordKindContainerLog}, resource, api.StreamMetadata{ContainerLogDirectoryMetadata: "/var/log/pods/ns_pod_uid/sandbox"}); err == nil {
		t.Fatal("ResolveFamily() accepted a StreamRef for a different resource")
	}
}

type outsideClusterFormat struct{}

func (outsideClusterFormat) Kind() api.RecordKind { return "outside-cluster-test" }
func (outsideClusterFormat) ContentType() string  { return "application/octet-stream" }
func (outsideClusterFormat) EncodeBatch(api.Batch) ([]byte, error) {
	return []byte("event\n"), nil
}
func (outsideClusterFormat) ObjectFamily(api.StreamRef, api.Resource, api.StreamMetadata) (objectlayout.Family, error) {
	return objectlayout.NewFamily("", []string{"another-cluster", "stream"}, "events", ".bin")
}
func (outsideClusterFormat) ObjectMetadata(api.Resource, api.StreamMetadata) (map[string]string, error) {
	return nil, nil
}

type wrongKindNamespaceFormat struct{}

func (wrongKindNamespaceFormat) Kind() api.RecordKind { return "wrong-kind-namespace-test" }
func (wrongKindNamespaceFormat) ContentType() string  { return "application/octet-stream" }
func (wrongKindNamespaceFormat) EncodeBatch(api.Batch) ([]byte, error) {
	return []byte("event\n"), nil
}
func (wrongKindNamespaceFormat) ObjectFamily(api.StreamRef, api.Resource, api.StreamMetadata) (objectlayout.Family, error) {
	return objectlayout.NewFamily("", []string{"prod", "custom"}, "events", ".bin")
}
func (wrongKindNamespaceFormat) ObjectMetadata(api.Resource, api.StreamMetadata) (map[string]string, error) {
	return nil, nil
}

type emptyEncodingFormat struct{}

func (emptyEncodingFormat) Kind() api.RecordKind { return "empty-encoding-test" }
func (emptyEncodingFormat) ContentType() string  { return "application/octet-stream" }
func (emptyEncodingFormat) EncodeBatch(api.Batch) ([]byte, error) {
	return nil, nil
}
func (emptyEncodingFormat) ObjectFamily(api.StreamRef, api.Resource, api.StreamMetadata) (objectlayout.Family, error) {
	return objectlayout.NewFamily("", []string{"cluster", "stream"}, "events", ".bin")
}
func (emptyEncodingFormat) ObjectMetadata(api.Resource, api.StreamMetadata) (map[string]string, error) {
	return nil, nil
}

func TestResolveFamilyOwnsPrefixAndClusterBoundary(t *testing.T) {
	resource := api.Resource{ClusterName: "prod"}
	ref := api.StreamRef{ID: "test/stream", Kind: "outside-cluster-test"}
	if _, err := ResolveFamily(outsideClusterFormat{}, "logs", ref, resource, nil); err == nil {
		t.Fatal("ResolveFamily() accepted a family outside the configured cluster root")
	}
	wrongNamespace := wrongKindNamespaceFormat{}
	if _, err := ResolveFamily(wrongNamespace, "logs", api.StreamRef{ID: "test/stream", Kind: wrongNamespace.Kind()}, resource, nil); err == nil {
		t.Fatal("ResolveFamily() accepted a non-container-log family outside its record-kind namespace")
	}
	format, err := Lookup(api.RecordKindContainerLog)
	if err != nil {
		t.Fatal(err)
	}
	metadata := api.StreamMetadata{ContainerLogDirectoryMetadata: "/var/log/pods/ns_pod_uid/sandbox"}
	family, err := ResolveFamily(format, "logs/nodes", api.StreamRef{ID: "container-logs/uid/sandbox", Kind: api.RecordKindContainerLog}, api.Resource{ClusterName: "prod", Namespace: "ns", SandboxID: "sb", PodUID: "uid", Container: "sandbox"}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if got := family.DataKey(0); got != "logs/nodes/prod/ns/sb/uid/sandbox.log" {
		t.Fatalf("resolved data key=%q", got)
	}
}

func TestEncodeBatchRejectsMissingFormatAndKindDrift(t *testing.T) {
	resource := api.Resource{SandboxID: "sb"}
	for _, test := range []struct {
		name  string
		batch api.Batch
		want  string
	}{
		{name: "missing", batch: api.Batch{StreamRef: api.StreamRef{Kind: "missing"}, Items: []api.BatchItem{{Record: api.Record{Kind: "missing", Resource: resource}}}}, want: "no stream format"},
		{name: "drift", batch: api.Batch{StreamRef: api.StreamRef{Kind: api.RecordKindContainerLog}, Items: []api.BatchItem{{Record: api.Record{Kind: "other", Resource: resource}}}}, want: "does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := EncodeBatch(test.batch); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("EncodeBatch() error=%v", err)
			}
		})
	}
}

func TestEncodeBatchRejectsEmptyFormatOutput(t *testing.T) {
	format := emptyEncodingFormat{}
	Register(format)
	batch := api.Batch{
		StreamRef: api.StreamRef{ID: "test/stream", Kind: format.Kind()},
		Items:     []api.BatchItem{{Record: api.Record{Kind: format.Kind(), Resource: api.Resource{ClusterName: "cluster"}}}},
	}
	if _, _, _, err := EncodeBatch(batch); err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("EncodeBatch() error=%v", err)
	}
}
