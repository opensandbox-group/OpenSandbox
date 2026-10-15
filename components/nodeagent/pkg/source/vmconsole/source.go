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

// Package vmconsole collects Firecracker's host-side guest serial output.
package vmconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/internal/fileidentity"
	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	"github.com/alibaba/opensandbox/nodeagent/pkg/config"
	"github.com/alibaba/opensandbox/nodeagent/pkg/registry"
	"github.com/alibaba/opensandbox/nodeagent/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var sandboxGVR = schema.GroupVersionResource{Group: "sandbox.fast.io", Version: "v1alpha2", Resource: "sandboxes"}

type stream struct {
	Device      uint64
	Inode       uint64
	Finalizing  bool
	EndedAt     time.Time
	UID         string
	Resource    api.Resource
	Path        string
	Offset      int64
	Sequence    uint64
	Prefix      string
	PrefixBytes int
	Started     time.Time
	Outcome     api.SourceOutcome
	Ended       bool
	file        *os.File
	pending     string
	ending      bool
}

type token struct {
	UID      string
	Sequence uint64
	Offset   int64
}

type source struct {
	cfg       config.Config
	state     registry.SourceState
	pods      cache.SharedIndexInformer
	sandboxes cache.SharedIndexInformer
	onError   func(error)
	mu        sync.Mutex
	streams   map[string]*stream
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	out       chan<- api.SourceEvent
}

func init() {
	registry.RegisterSource(api.SourceNameVMConsole, func(d registry.SourceDependencies) (api.Source, error) {
		if d.KubernetesConfig == nil {
			return nil, errors.New("vm-console requires Kubernetes configuration")
		}
		client, err := kubernetes.NewForConfig(d.KubernetesConfig)
		if err != nil {
			return nil, err
		}
		dyn, err := dynamic.NewForConfig(d.KubernetesConfig)
		if err != nil {
			return nil, err
		}
		pods := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", d.Config.NodeName).String()
		})).Core().V1().Pods().Informer()
		sandboxes := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0).ForResource(sandboxGVR).Informer()
		return &source{cfg: d.Config, state: d.State, pods: pods, sandboxes: sandboxes, onError: d.OnError, streams: make(map[string]*stream), done: make(chan struct{}), wake: make(chan struct{}, 1)}, nil
	})
}

func (s *source) Capabilities() api.Capabilities {
	return api.Capabilities{RecordKinds: []api.RecordKind{api.RecordKindVMConsole}}
}

func (s *source) Start(ctx context.Context, out chan<- api.SourceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("vm-console already started")
	}
	if err := s.state.View(func(r state.SourceStateReader) error {
		return r.ForEach(func(key, value []byte) error {
			var st stream
			if err := json.Unmarshal(value, &st); err != nil {
				return err
			}
			if !safeUID(st.UID) || string(key) != st.UID || st.Offset < 0 || st.Sequence == 0 || st.Started.IsZero() || st.PrefixBytes < 0 || st.PrefixBytes > 256 {
				return errors.New("invalid VM console checkpoint")
			}
			if st.Path != "" {
				relative, err := filepath.Rel(s.cfg.VMConsoleRoot, st.Path)
				if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return errors.New("VM console checkpoint belongs to a different input root")
				}
			}
			s.streams[st.UID] = &st
			return nil
		})
	}); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.out = cancel, out
	go s.pods.Run(runCtx.Done())
	go s.sandboxes.Run(runCtx.Done())
	syncCtx, syncCancel := context.WithTimeout(runCtx, time.Minute)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), s.pods.HasSynced, s.sandboxes.HasSynced) {
		cancel()
		s.cancel = nil
		return errors.New("vm-console Kubernetes caches did not sync")
	}
	go s.run(runCtx)
	return nil
}

func (s *source) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *source) save(st *stream) error {
	value, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.state.Update(func(w state.SourceStateWriter) error { return w.Put([]byte(st.UID), value) })
}

func (s *source) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *source) Acknowledge(_ context.Context, results []api.AckResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		var t token
		if err := json.Unmarshal(result.Token.Value, &t); err != nil {
			return api.Permanent(err)
		}
		st := s.streams[t.UID]
		if st == nil || result.Token.Source != api.SourceNameVMConsole || result.Token.StreamRef != streamRef(t.UID) || result.Token.ID != tokenID(t) || st.Sequence != t.Sequence {
			return api.Permanent(errors.New("invalid VM console acknowledgement"))
		}
		if t.Offset == st.Offset {
			continue
		}
		if st.pending != result.Token.ID || t.Offset < st.Offset {
			return api.Permanent(errors.New("unexpected VM console offset"))
		}
		next := *st
		next.Offset = t.Offset
		if result.Disposition == api.AckIntentionalDrop {
			next.Outcome.HadDrops = true
			next.Outcome.LossReasons = api.AddLossReason(next.Outcome.LossReasons, result.Reason)
		}
		if err := s.save(&next); err != nil {
			return err
		}
		*st = next
		st.pending = ""
	}
	s.notify()
	return nil
}

func (s *source) AcknowledgeEnd(_ context.Context, t api.EndToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value token
	if err := json.Unmarshal(t.Value, &value); err != nil {
		return api.Permanent(err)
	}
	st := s.streams[value.UID]
	if st == nil || t.Source != api.SourceNameVMConsole || t.StreamRef != streamRef(value.UID) || t.ID != "end/"+tokenID(value) || value.Sequence != st.Sequence || value.Offset != st.Offset || !st.Finalizing && !st.Ended {
		return api.Permanent(errors.New("invalid VM console end acknowledgement"))
	}
	next := *st
	next.Ended = true
	next.EndedAt = time.Now().UTC()
	if err := s.save(&next); err != nil {
		return err
	}
	*st = next
	s.notify()
	return nil
}

func streamRef(uid string) api.StreamRef {
	return api.StreamRef{ID: path.Join(api.SourceNameVMConsole, uid), Kind: api.RecordKindVMConsole}
}

func tokenID(t token) string {
	return fmt.Sprintf("%s/%d/%d", t.UID, t.Sequence, t.Offset)
}
func metadata(st *stream) api.StreamMetadata {
	return api.StreamMetadata{"sandbox-uid": st.UID}
}

func (s *source) run(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, st := range s.streams {
			if st.file != nil {
				_ = st.file.Close()
				st.file = nil
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if s.onError != nil {
				s.onError(err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// A UID is taken from the CR, never inferred from a truncated jail name.
func (s *source) localSandbox(cr *unstructured.Unstructured) (api.Resource, bool) {
	name, _, _ := unstructured.NestedString(cr.Object, "status", "placement", "fastletName")
	uid, _, _ := unstructured.NestedString(cr.Object, "status", "placement", "fastletPodUID")
	obj, exists, err := s.pods.GetStore().GetByKey(cr.GetNamespace() + "/" + name)
	if err != nil || !exists {
		return api.Resource{}, false
	}
	pod := obj.(*corev1.Pod)
	if pod.Spec.NodeName != s.cfg.NodeName || uid == "" || string(pod.UID) != uid {
		return api.Resource{}, false
	}
	return api.Resource{SandboxID: cr.GetName(), Namespace: cr.GetNamespace(), ClusterName: s.cfg.ClusterID, NodeName: s.cfg.NodeName, PodName: pod.Name, PodUID: string(pod.UID), Container: "fastlet"}, true
}

func safeUID(uid string) bool {
	return uid != "" && uid != "." && uid != ".." && !strings.ContainsAny(uid, "/\\")
}

// A retained terminal CR is discoverable only during its bounded catch-up window.
// Without a transition timestamp, rediscovery cannot be distinguished from replay.
func terminalExpired(cr *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(cr.Object, "status", "runtime", "state")
	if phase != "Stopped" && phase != "Failed" {
		return false
	}
	value, _, _ := unstructured.NestedString(cr.Object, "status", "runtime", "lastTransitionTime")
	changed, err := time.Parse(time.RFC3339Nano, value)
	return err != nil || time.Since(changed) >= 24*time.Hour
}

func (s *source) scan(ctx context.Context) error {
	seen := make(map[string]*unstructured.Unstructured)
	s.mu.Lock()
	for _, obj := range s.sandboxes.GetStore().List() {
		cr := obj.(*unstructured.Unstructured)
		uid := string(cr.GetUID())
		if !safeUID(uid) {
			continue
		}
		seen[uid] = cr
		if terminalExpired(cr) {
			continue
		}
		if _, exists := s.streams[uid]; exists {
			continue
		}
		resource, local := s.localSandbox(cr)
		if !local {
			continue
		}
		st := &stream{UID: uid, Resource: resource, Started: time.Now().UTC().Truncate(time.Second), Sequence: 1}
		if err := s.save(st); err != nil {
			s.mu.Unlock()
			return err
		}
		s.streams[uid] = st
	}
	uids := make([]string, 0, len(s.streams))
	for uid := range s.streams {
		uids = append(uids, uid)
	}
	s.mu.Unlock()
	for _, uid := range uids {
		s.mu.Lock()
		st := s.streams[uid]
		cr := seen[st.UID]
		if st.Ended {
			if (cr == nil || terminalExpired(cr)) && time.Since(st.EndedAt) > 24*time.Hour {
				if err := s.state.Update(func(w state.SourceStateWriter) error { return w.Delete([]byte(uid)) }); err != nil {
					s.mu.Unlock()
					return err
				}
				delete(s.streams, uid)
			}
			s.mu.Unlock()
			continue
		}
		phase := ""
		if cr != nil {
			phase, _, _ = unstructured.NestedString(cr.Object, "status", "runtime", "state")
		}
		ended := cr == nil || phase == "Stopped" || phase == "Failed"
		event, err := s.read(st, ended)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if event != nil {
			select {
			case s.out <- *event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func (s *source) open(st *stream) error {
	uid32 := st.UID
	if len(uid32) > 32 {
		uid32 = uid32[:32]
	}
	candidates := []string{filepath.Join(s.cfg.VMConsoleRoot, "jails", "firecracker", uid32, "root", "firecracker.log"), filepath.Join(s.cfg.VMConsoleRoot, "sandboxes", st.UID, "firecracker.log")}
	if st.Path != "" {
		candidates = []string{st.Path}
	}
	for _, name := range candidates {
		file, err := os.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			return errors.New("VM console is not a regular file")
		}
		device, inode, err := fileidentity.Read(info)
		if err != nil {
			_ = file.Close()
			return err
		}
		if st.Inode != 0 && (st.Device != device || st.Inode != inode) {
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Sequence++
			gap(st, "vm-console-file-replaced")
		}
		st.Device, st.Inode = device, inode
		st.file, st.Path = file, name
		return nil
	}
	return nil
}

func gap(st *stream, reason string) {
	st.Outcome.HadSourceGaps = true
	st.Outcome.LossReasons = api.AddLossReason(st.Outcome.LossReasons, reason)
}

func (s *source) read(st *stream, ended bool) (*api.SourceEvent, error) {
	if st.pending != "" || st.ending {
		return nil, nil
	}
	if st.Finalizing {
		return s.endEvent(st), nil
	}
	before := *st
	if st.file == nil {
		if err := s.open(st); err != nil {
			return nil, err
		}
	}
	if st.file != nil {
		info, err := st.file.Stat()
		if err != nil {
			return nil, err
		}
		// Keep the open descriptor after unlink so the final tail survives driver GC.
		current, statErr := os.Stat(st.Path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		replaced := current != nil && !os.SameFile(info, current)
		// Drain the old descriptor before following a replacement pathname.
		if replaced && info.Size() <= st.Offset {
			_ = st.file.Close()
			st.file = nil
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Device, st.Inode = 0, 0
			st.Sequence++
			gap(st, "vm-console-file-replaced")
			if err := s.save(st); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if info.Size() < st.Offset {
			st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
			st.Sequence++
			gap(st, "vm-console-file-truncated")
		}
		if st.PrefixBytes > 0 {
			prefix := make([]byte, st.PrefixBytes)
			n, err := st.file.ReadAt(prefix, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			hash := sha256.Sum256(prefix[:n])
			if hex.EncodeToString(hash[:]) != st.Prefix {
				st.Offset, st.PrefixBytes, st.Prefix = 0, 0, ""
				st.Sequence++
				gap(st, "vm-console-file-replaced")
			}
		}
		if st.PrefixBytes == 0 && info.Size() > 0 {
			prefix := make([]byte, min(info.Size(), 256))
			n, err := st.file.ReadAt(prefix, 0)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			hash := sha256.Sum256(prefix[:n])
			st.PrefixBytes, st.Prefix = n, hex.EncodeToString(hash[:])
		}
		if st.Path != before.Path || st.Device != before.Device || st.Inode != before.Inode || st.Sequence != before.Sequence || st.Prefix != before.Prefix || st.PrefixBytes != before.PrefixBytes || !st.Outcome.Equal(before.Outcome) {
			if err := s.save(st); err != nil {
				return nil, err
			}
		}
		if info.Size() > st.Offset {
			chunkSize := min(int64(64<<10), s.cfg.PerSandboxQueueBytes-1024)
			if chunkSize <= 0 {
				return nil, errors.New("VM console queue budget is too small")
			}
			body := make([]byte, chunkSize)
			n, err := st.file.ReadAt(body, st.Offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if n > 0 {
				t := token{UID: st.UID, Sequence: st.Sequence, Offset: st.Offset + int64(n)}
				value, _ := json.Marshal(t)
				st.pending = tokenID(t)
				return &api.SourceEvent{Delivery: &api.Delivery{Record: api.Record{Kind: api.RecordKindVMConsole, Timestamp: time.Now().UTC(), Resource: st.Resource, Body: body[:n]}, StreamRef: streamRef(st.UID), Metadata: metadata(st), RecordID: tokenID(t), AckToken: api.AckToken{Source: api.SourceNameVMConsole, ID: tokenID(t), StreamRef: streamRef(st.UID), Value: value}}}, nil
			}
		}
	}
	if !ended {
		return nil, nil
	}
	if st.file == nil {
		gap(st, "vm-console-file-unavailable")
	} else {
		_ = st.file.Close()
		st.file = nil
	}
	st.Finalizing = true
	if err := s.save(st); err != nil {
		return nil, err
	}
	return s.endEvent(st), nil
}

func (s *source) endEvent(st *stream) *api.SourceEvent {
	t := token{UID: st.UID, Sequence: st.Sequence, Offset: st.Offset}
	value, _ := json.Marshal(t)
	st.ending = true
	return &api.SourceEvent{End: &api.StreamEnd{StreamRef: streamRef(st.UID), Resource: st.Resource, Metadata: metadata(st), Revision: 1, CoverageStartedAt: st.Started, Outcome: st.Outcome.Clone(), EndToken: api.EndToken{Source: api.SourceNameVMConsole, ID: "end/" + tokenID(t), StreamRef: streamRef(st.UID), Value: value}}}
}
