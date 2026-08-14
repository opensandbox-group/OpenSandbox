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

//go:build integration

package oss

import (
	"bytes"
	"context"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/identity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/marker"
	lineformat "github.com/alibaba/opensandbox/nodeagent/pkg/sink"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

func TestRealOSSSmoke(t *testing.T) {
	endpoint := os.Getenv("NODEAGENT_TEST_OSS_ENDPOINT")
	bucket := os.Getenv("NODEAGENT_TEST_OSS_BUCKET")
	basePrefix := os.Getenv("NODEAGENT_TEST_OSS_PREFIX")
	accessKeyID := os.Getenv("OSS_ACCESS_KEY_ID")
	accessKeySecret := os.Getenv("OSS_ACCESS_KEY_SECRET")
	if endpoint == "" || bucket == "" || basePrefix == "" || accessKeyID == "" || accessKeySecret == "" {
		t.Skip("real OSS test credentials and disposable prefix are not configured")
	}
	prefix := path.Join(basePrefix, "run-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	targetID, err := identity.OSSTargetID(endpoint, bucket, prefix, "integration")
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.TempDir(), targetID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sink, err := New(Config{Endpoint: endpoint, Bucket: bucket, Prefix: prefix, ClusterID: "integration", AccessKeyID: accessKeyID, AccessKeySecret: accessKeySecret, SessionToken: os.Getenv("OSS_SESSION_TOKEN"), WriterID: db.WriterID(), TargetID: targetID, MaxObjectBytes: 1 << 20, Timeout: 30 * time.Second}, db)
	if err != nil {
		t.Fatal(err)
	}
	resource := api.Resource{SandboxID: "sb-smoke", ClusterName: "integration", Namespace: "default", PodName: "pod", PodUID: "uid-" + strconv.FormatInt(time.Now().UnixNano(), 10), NodeName: "node", Container: "sandbox", LogDirectory: "/var/log/pods/default_pod_uid/sandbox"}
	streamRef := api.StreamRef{ID: "container-logs/" + resource.PodUID + "/sandbox"}
	batch := api.Batch{StreamRef: streamRef, Items: []api.BatchItem{{RecordID: "record", Record: api.Record{Kind: api.RecordKindContainerLog, Timestamp: time.Now().UTC(), Body: []byte("hello"), Resource: resource, Attributes: map[string]string{"stream": "stdout"}}}}}
	if err := sink.Consume(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	request := api.FinalizeRequest{FinalizeID: identity.FinalizeID(streamRef.ID, 1, targetID), TargetID: targetID, StreamRef: streamRef, Revision: 1, CoverageStartedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Resource: resource, FinalizedAt: time.Now().UTC().Truncate(time.Second)}
	if err := sink.Finalize(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	dataKey := objectKey(prefix, resource, 0)
	data, err := sink.backend.Get(context.Background(), dataKey)
	if err != nil || !bytes.Equal(data, lineformat.EncodeBatch(batch)) {
		t.Fatalf("data=%q err=%v", data, err)
	}
	metadata, err := sink.backend.Head(context.Background(), dataKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sink.backend.Get(context.Background(), markerKey(prefix, resource, 1))
	if err != nil {
		t.Fatal(err)
	}
	value, err := marker.Decode(raw)
	if err != nil || len(value.Objects) != 1 || value.Objects[0].Size != metadata.Size || value.Objects[0].CRC64 != metadata.CRC64 {
		t.Fatalf("marker=%+v metadata=%+v err=%v", value, metadata, err)
	}
	t.Logf("real OSS smoke objects retained for offline cleanup: target-id=%s family-prefix=%s container=%s", targetID, path.Dir(dataKey), resource.Container)
}
