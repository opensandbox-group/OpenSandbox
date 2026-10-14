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

package vmconsole

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

func fixture(t *testing.T) (*source, *stream, string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("file identity requires Linux or Darwin")
	}
	root := t.TempDir()
	db, err := state.Open(t.TempDir(), "test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	private, err := db.SourceState(api.SourceNameVMConsole)
	if err != nil {
		t.Fatal(err)
	}
	s := &source{cfg: config.Config{VMConsoleRoot: root, PerSandboxQueueBytes: 1 << 20, NodeName: "node-a", ClusterID: "test"}, state: private, streams: make(map[string]*stream), wake: make(chan struct{}, 1)}
	st := &stream{UID: "12345678-1234-1234-1234-123456789012", Attempt: 1, Sequence: 1, Started: time.Now().UTC().Truncate(time.Second), Resource: api.Resource{SandboxID: "sandbox-a", ClusterName: "test", Namespace: "ns", PodName: "fastlet", PodUID: "pod-a", NodeName: "node-a", Container: "fastlet"}}
	s.streams[streamKey(st.UID, st.Attempt)] = st
	filename := filepath.Join(root, "sandboxes", st.UID, "firecracker.log")
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if st.file != nil {
			_ = st.file.Close()
		}
	})
	return s, st, filename
}

func acknowledge(t *testing.T, s *source, e *api.SourceEvent) {
	t.Helper()
	if e == nil || e.Delivery == nil {
		t.Fatal("expected a delivery")
	}
	if err := s.Acknowledge(context.Background(), []api.AckResult{{Token: e.Delivery.AckToken, Disposition: api.AckDelivered, Guarantee: api.GuaranteeDurable}}); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleResumeAndDriverGC(t *testing.T) {
	s, st, filename := fixture(t)
	if err := os.WriteFile(filename, []byte("hello\r\n\x00guest"), 0640); err != nil {
		t.Fatal(err)
	}
	e, err := s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.Delivery.Record.Body) != "hello\r\n\x00guest" || st.Offset != 0 {
		t.Fatal("console bytes or unacknowledged offset changed")
	}
	acknowledge(t, s, e)
	_ = st.file.Close()
	st.file = nil
	var restored stream
	if err := s.state.View(func(r state.SourceStateReader) error {
		value, _, err := r.Get([]byte(streamKey(st.UID, st.Attempt)))
		if err != nil {
			return err
		}
		return json.Unmarshal(value, &restored)
	}); err != nil {
		t.Fatal(err)
	}
	*st = restored
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("tail")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.Delivery.Record.Body) != "tail" {
		t.Fatal("restart replayed acknowledged bytes")
	}
	acknowledge(t, s, e)
	// GC removes the pathname while the collector still owns an open descriptor.
	if err := os.Remove(filename); err != nil {
		t.Skipf("host does not support unlinking open files: %v", err)
	}
	e, err = s.read(st, true)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.End == nil || e.End.Outcome.HadSourceGaps {
		t.Fatal("readable tail should finalize without a source gap")
	}
	if err := s.AcknowledgeEnd(context.Background(), e.End.EndToken); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleMissingFileAndFinalizationRecovery(t *testing.T) {
	s, st, _ := fixture(t)
	e, err := s.read(st, true)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || !e.End.Outcome.HadSourceGaps {
		t.Fatal("missing console must report a source gap")
	}
	st.ending = false
	again, err := s.read(st, true)
	if err != nil {
		t.Fatal(err)
	}
	if !again.End.EndToken.Equal(e.End.EndToken) || !again.End.Outcome.Equal(e.End.Outcome) {
		t.Fatal("recovered finalization changed")
	}
	if err := s.AcknowledgeEnd(context.Background(), e.End.EndToken); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleReplacementAfterRestart(t *testing.T) {
	s, st, filename := fixture(t)
	if err := os.WriteFile(filename, []byte("same-prefix"), 0640); err != nil {
		t.Fatal(err)
	}
	e, err := s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	acknowledge(t, s, e)
	_ = st.file.Close()
	st.file = nil
	// Keep the old inode allocated so replacement cannot reuse it.
	if err := os.Rename(filename, filename+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("same-prefix-new-instance"), 0640); err != nil {
		t.Fatal(err)
	}
	e, err = s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || string(e.Delivery.Record.Body) != "same-prefix-new-instance" || !st.Outcome.HadSourceGaps {
		t.Fatal("replaced file incorrectly reused the previous offset")
	}
}

func TestSandboxIdentityAndPlacement(t *testing.T) {
	s, _, _ := fixture(t)
	s.pods = cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fastlet", Namespace: "ns", UID: types.UID("pod-a")}, Spec: corev1.PodSpec{NodeName: "node-a"}}
	if err := s.pods.GetStore().Add(pod); err != nil {
		t.Fatal(err)
	}
	cr := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "public-sandbox-id", "namespace": "ns", "uid": "vm-uid"}, "status": map[string]interface{}{"placement": map[string]interface{}{"fastletName": "fastlet", "fastletPodUID": "pod-a"}}}}
	resource, ok := s.localSandbox(cr)
	if !ok || resource.SandboxID != "public-sandbox-id" || resource.PodUID != "pod-a" {
		t.Fatal("sandbox name must define its public ID")
	}
	_ = unstructured.SetNestedField(cr.Object, "stale-pod", "status", "placement", "fastletPodUID")
	if _, ok := s.localSandbox(cr); ok {
		t.Fatal("stale placement was accepted")
	}
	_ = unstructured.SetNestedField(cr.Object, "pod-a", "status", "placement", "fastletPodUID")
	s.cfg.NodeName = "node-b"
	if _, ok := s.localSandbox(cr); ok {
		t.Fatal("another node's sandbox was accepted")
	}
}
