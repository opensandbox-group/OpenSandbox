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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
)

func fixture(t *testing.T) (*source, *stream, string) {
	t.Helper()
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
	st := &stream{UID: "12345678-1234-1234-1234-123456789012", Sequence: 1, Started: time.Now().UTC().Truncate(time.Second), Resource: api.Resource{SandboxID: "sandbox-a", ClusterName: "test", Namespace: "ns", NodeName: "node-a"}}
	s.streams[st.UID] = st
	filename := filepath.Join(root, "sandboxes", st.UID, "firecracker.log")
	st.Path = filename
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, current := range s.streams {
			if current.file != nil {
				_ = current.file.Close()
			}
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
	requireFileIdentity(t)
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
		value, _, err := r.Get([]byte(st.UID))
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
	requireFileIdentity(t)
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
	requireFileIdentity(t)
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

func requireFileIdentity(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("file identity requires Linux or Darwin")
	}
}

func TestRestartWithPendingConsoleSwitch(t *testing.T) {
	requireFileIdentity(t)
	for _, old := range []string{"survives", "removed", "replaced"} {
		t.Run(old, func(t *testing.T) {
			s, st, filename := fixture(t)
			payload := bytes.Repeat([]byte("a"), 150000)
			if err := os.WriteFile(filename, payload, 0640); err != nil {
				t.Fatal(err)
			}
			first, err := s.read(st, false)
			if err != nil {
				t.Fatal(err)
			}
			acknowledge(t, s, first)
			next := filepath.Join(s.cfg.VMConsoleRoot, "jails", "firecracker", st.UID[:32], "root", "firecracker.log")
			if err := os.MkdirAll(filepath.Dir(next), 0750); err != nil {
				t.Fatal(err)
			}
			writeMetadata(t, next, st, 2)
			if err := os.WriteFile(next, []byte("new"), 0640); err != nil {
				t.Fatal(err)
			}
			st.NextPath = next
			if err := s.save(st); err != nil {
				t.Fatal(err)
			}
			_ = st.file.Close()
			st.file = nil
			if old != "survives" {
				if err := os.Rename(filename, filename+".retired"); err != nil {
					t.Fatal(err)
				}
				if old == "replaced" {
					if err := os.WriteFile(filename, []byte("wrong inode"), 0640); err != nil {
						t.Fatal(err)
					}
				}
			}
			restored := &source{cfg: s.cfg, state: s.state, streams: make(map[string]*stream), done: make(chan struct{}), wake: make(chan struct{}, 1)}
			out := make(chan api.SourceEvent, 4)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				if err := restored.Stop(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if err := restored.Start(ctx, out); err != nil {
				t.Fatal(err)
			}
			var got []byte
			want := []byte("new")
			if old == "survives" {
				want = append(append([]byte(nil), payload[len(first.Delivery.Record.Body):]...), want...)
			}
			for len(got) < len(want) {
				select {
				case event := <-out:
					if event.Delivery == nil || event.Delivery.StreamRef != first.Delivery.StreamRef {
						t.Fatal("stream changed during recovery")
					}
					got = append(got, event.Delivery.Record.Body...)
					acknowledge(t, restored, &event)
				case <-time.After(5 * time.Second):
					t.Fatal("recovery stalled")
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatal("old tail was skipped or replacement was read out of order")
			}
		})
	}
}

func writeMetadata(t *testing.T, filename string, st *stream, attempt int64) {
	t.Helper()
	data, err := json.Marshal(vmMetadata{Version: 1, UID: st.UID, Namespace: st.Resource.Namespace, Name: st.Resource.SandboxID, PlacementAttempt: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(filename), "fast-sandbox.json"), data, 0640); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataDiscoveryRetriesAndSelectsNewest(t *testing.T) {
	s, st, direct := fixture(t)
	jail := filepath.Join(s.cfg.VMConsoleRoot, "jails", "firecracker", st.UID[:32], "root", "firecracker.log")
	if err := os.MkdirAll(filepath.Dir(jail), 0750); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{direct, jail} {
		meta := filepath.Join(filepath.Dir(filename), "fast-sandbox.json")
		for _, data := range []string{"", `{`, `{"version":2,"uid":"` + st.UID + `","namespace":"ns","name":"sandbox-a"}`} {
			if err := os.WriteFile(meta, []byte(data), 0640); err != nil {
				t.Fatal(err)
			}
			found, err := s.discover()
			if err != nil || len(found) != 0 {
				t.Fatalf("invalid publication discovered: %v, %v", found, err)
			}
		}
		if err := os.Remove(meta); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata(t, direct, st, 1)
	writeMetadata(t, jail, st, 2)
	found, err := s.discover()
	if err != nil || len(found) != 1 || found[st.UID].path != jail {
		t.Fatalf("newest local directory not selected: %v, %v", found, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(jail), "fast-sandbox.json")); err != nil {
		t.Fatal(err)
	}
	found, err = s.discover()
	if err != nil || found[st.UID].path != direct {
		t.Fatal("metadata disappearance was fatal or blocked retry")
	}
}

func TestLocalDiscoveryResumeGraceAndGC(t *testing.T) {
	requireFileIdentity(t)
	s, original, filename := fixture(t)
	delete(s.streams, original.UID)
	out := make(chan api.SourceEvent, 16)
	s.out = out
	writeMetadata(t, filename, original, 3)
	if err := os.WriteFile(filename, []byte("first"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := <-out
	if first.Delivery == nil || first.Delivery.Record.Resource.PodUID != "" {
		t.Fatal("local discovery fabricated Pod identity")
	}
	acknowledge(t, s, &first)
	st := s.streams[original.UID]
	if err := os.RemoveAll(filepath.Dir(filename)); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Finalizing || st.MissingSince.IsZero() {
		t.Fatal("missing directory finalized without grace")
	}
	// Recreate at a different supported layout inside grace; drain the old fd first.
	jail := filepath.Join(s.cfg.VMConsoleRoot, "jails", "firecracker", st.UID[:32], "root", "firecracker.log")
	if err := os.MkdirAll(filepath.Dir(jail), 0750); err != nil {
		t.Fatal(err)
	}
	writeMetadata(t, jail, st, 4)
	if err := os.WriteFile(jail, []byte("resumed"), 0640); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	resumed := <-out
	if resumed.Delivery == nil || string(resumed.Delivery.Record.Body) != "resumed" || resumed.Delivery.StreamRef != first.Delivery.StreamRef || len(s.streams) != 1 || !st.MissingSince.IsZero() {
		t.Fatal("returning directory split or failed to resume stream")
	}
	acknowledge(t, s, &resumed)
	if err := os.RemoveAll(filepath.Dir(jail)); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.MissingSince = time.Now().Add(-disappearanceGrace - time.Second)
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	end := <-out
	if end.End == nil {
		t.Fatal("missing directory never finalized")
	}
	if err := s.AcknowledgeEnd(context.Background(), end.End.EndToken); err != nil {
		t.Fatal(err)
	}
	st.EndedAt = time.Now().Add(-25 * time.Hour)
	if err := s.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.streams) != 0 {
		t.Fatal("ended state not reclaimed")
	}
	if err := s.state.View(func(r state.SourceStateReader) error {
		_, exists, err := r.Get([]byte(st.UID))
		if exists {
			t.Error("ended checkpoint retained")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingMetadataKeepsReadableConsole(t *testing.T) {
	requireFileIdentity(t)
	s, st, filename := fixture(t)
	if err := os.WriteFile(filename, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	first, err := s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	acknowledge(t, s, first)
	next := filepath.Join(s.cfg.VMConsoleRoot, "jails", "firecracker", st.UID[:32], "root", "firecracker.log")
	if err := os.MkdirAll(filepath.Dir(next), 0750); err != nil {
		t.Fatal(err)
	}
	writeMetadata(t, next, st, 2)
	st.MissingSince = time.Now().Add(-disappearanceGrace - time.Second)
	out := make(chan api.SourceEvent, 2)
	s.out = out
	if err := s.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Finalizing || !st.MissingSince.IsZero() || len(out) != 0 {
		t.Fatal("pending console hid the readable old input")
	}
	// Also recover when the next file vanished after its path was selected.
	_ = st.file.Close()
	st.file, st.NextPath = nil, next
	if err := os.WriteFile(filename, []byte("oldtail"), 0640); err != nil {
		t.Fatal(err)
	}
	tail, err := s.read(st, false)
	if err != nil || tail == nil || tail.Delivery == nil || string(tail.Delivery.Record.Body) != "tail" {
		t.Fatalf("old input was not reopened: %v", err)
	}
	acknowledge(t, s, tail)
	if err := os.WriteFile(next, []byte("new"), 0640); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	current := <-out
	if current.Delivery == nil || string(current.Delivery.Record.Body) != "new" || current.Delivery.StreamRef != first.Delivery.StreamRef {
		t.Fatal("new input did not continue the same stream")
	}
}

func TestReplacementDrainsBeforeSwitch(t *testing.T) {
	requireFileIdentity(t)
	for _, tc := range []struct {
		name  string
		ended bool
	}{{"running", false}, {"terminal", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, filename := fixture(t)
			payload := bytes.Repeat([]byte("a"), 70000)
			if err := os.WriteFile(filename, payload, 0640); err != nil {
				t.Fatal(err)
			}
			first, err := s.read(st, tc.ended)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filename, filename+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, []byte("replacement"), 0640); err != nil {
				t.Fatal(err)
			}
			acknowledge(t, s, first)
			tail, err := s.read(st, tc.ended)
			if err != nil {
				t.Fatal(err)
			}
			if tail == nil || !bytes.Equal(tail.Delivery.Record.Body, payload[len(first.Delivery.Record.Body):]) {
				t.Fatal("replacement discarded readable old tail")
			}
			acknowledge(t, s, tail)
			if _, err := s.read(st, tc.ended); err != nil {
				t.Fatal(err)
			}
			current, err := s.read(st, tc.ended)
			if err != nil {
				t.Fatal(err)
			}
			if current == nil || string(current.Delivery.Record.Body) != "replacement" || !st.Outcome.HadSourceGaps {
				t.Fatal("replacement not collected with gap")
			}
		})
	}
}

type countingState struct {
	registry.SourceState
	writes int
}

func (c *countingState) Update(fn func(state.SourceStateWriter) error) error {
	c.writes++
	return c.SourceState.Update(fn)
}

func TestIdleConsoleDoesNotWriteCheckpoint(t *testing.T) {
	requireFileIdentity(t)
	s, st, filename := fixture(t)
	counter := &countingState{SourceState: s.state}
	s.state = counter
	if err := os.WriteFile(filename, []byte("data"), 0640); err != nil {
		t.Fatal(err)
	}
	event, err := s.read(st, false)
	if err != nil {
		t.Fatal(err)
	}
	acknowledge(t, s, event)
	writes := counter.writes
	for range 3 {
		event, err := s.read(st, false)
		if err != nil || event != nil {
			t.Fatalf("unexpected idle result: %v", err)
		}
	}
	if counter.writes != writes {
		t.Fatal("idle poll wrote checkpoint")
	}
}
