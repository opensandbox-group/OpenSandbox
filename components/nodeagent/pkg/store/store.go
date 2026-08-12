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

// Package store provides the node-local Kubernetes identity and lifecycle view.
package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/nodeagent/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	SandboxIDLabel = "opensandbox.io/id"
	PoolNameLabel  = "sandbox.opensandbox.io/pool-name"
	ContainerName  = "sandbox"
)

type View interface {
	List() []Resource
	GetByUID(string) (Resource, bool)
	Forget(string)
	Changes() <-chan struct{}
}

// Resource combines the stable identity exposed to the pipeline with the
// Kubernetes lifecycle state used only by the Store and Sources.
type Resource struct {
	api.Resource
	Terminated bool
}

type Store struct {
	nodeName  string
	clusterID string
	logRoot   string
	informer  cache.SharedIndexInformer

	mu            sync.RWMutex
	resources     map[string]Resource
	changes       chan struct{}
	watchFailedAt time.Time
}

func New(client kubernetes.Interface, nodeName, clusterID, logRoot string) *Store {
	s := &Store{
		nodeName:  nodeName,
		clusterID: clusterID,
		logRoot:   logRoot,
		resources: make(map[string]Resource),
		changes:   make(chan struct{}, 1),
	}
	selector := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = selector
			pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, options)
			if err == nil {
				s.markWatchSuccessful()
			} else {
				s.markWatchFailed()
			}
			return pods, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = selector
			watcher, err := client.CoreV1().Pods(metav1.NamespaceAll).Watch(ctx, options)
			if err != nil {
				s.markWatchFailed()
			} else {
				s.markWatchSuccessful()
			}
			return watcher, err
		},
	}
	s.informer = cache.NewSharedIndexInformer(lw, &corev1.Pod{}, 0, cache.Indexers{})
	_, _ = s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { s.upsert(obj) },
		UpdateFunc: func(_, current any) { s.upsert(current) },
		DeleteFunc: s.deleted,
	})
	_ = s.informer.SetWatchErrorHandler(func(_ *cache.Reflector, _ error) { s.markWatchFailed() })
	return s
}

func (s *Store) Start(ctx context.Context) error {
	go s.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), s.informer.HasSynced) {
		return fmt.Errorf("pod informer did not sync")
	}
	return nil
}

func (s *Store) Changes() <-chan struct{} { return s.changes }

func (s *Store) Stale(now time.Time, threshold time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.watchFailedAt.IsZero() && now.Sub(s.watchFailedAt) >= threshold
}

func (s *Store) markWatchFailed() {
	s.mu.Lock()
	if s.watchFailedAt.IsZero() {
		s.watchFailedAt = time.Now()
	}
	s.mu.Unlock()
}

func (s *Store) markWatchSuccessful() {
	s.mu.Lock()
	s.watchFailedAt = time.Time{}
	s.mu.Unlock()
}

func (s *Store) List() []Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Resource, 0, len(s.resources))
	for _, resource := range s.resources {
		out = append(out, resource)
	}
	return out
}

func (s *Store) GetByUID(uid string) (Resource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resource, ok := s.resources[uid]
	return resource, ok
}

func (s *Store) Forget(uid string) {
	s.mu.Lock()
	resource, ok := s.resources[uid]
	if ok && resource.Terminated {
		delete(s.resources, uid)
	}
	s.mu.Unlock()
}

func (s *Store) upsert(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName != s.nodeName {
		return
	}
	sandboxID := pod.Labels[SandboxIDLabel]
	_, pooled := pod.Labels[PoolNameLabel]
	if sandboxID == "" || pooled || !hasContainer(pod, ContainerName) {
		s.markTerminated(string(pod.UID))
		return
	}
	terminated := pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	resource := Resource{
		Resource: api.Resource{
			SandboxID:    sandboxID,
			ClusterName:  s.clusterID,
			Namespace:    pod.Namespace,
			PodName:      pod.Name,
			PodUID:       string(pod.UID),
			NodeName:     pod.Spec.NodeName,
			Container:    ContainerName,
			LogDirectory: filepath.Join(s.logRoot, fmt.Sprintf("%s_%s_%s", pod.Namespace, pod.Name, pod.UID), ContainerName),
		},
		Terminated: terminated,
	}
	s.mu.Lock()
	if previous, exists := s.resources[resource.PodUID]; exists && previous.SandboxID != resource.SandboxID {
		previous.Terminated = true
		s.resources[resource.PodUID] = previous
	} else {
		s.resources[resource.PodUID] = resource
	}
	s.mu.Unlock()
	s.notify()
}

func (s *Store) deleted(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown); tombstoneOK {
			pod, ok = tombstone.Obj.(*corev1.Pod)
		}
	}
	if ok {
		s.markTerminated(string(pod.UID))
	}
}

func (s *Store) markTerminated(uid string) {
	s.mu.Lock()
	resource, ok := s.resources[uid]
	if ok {
		resource.Terminated = true
		s.resources[uid] = resource
	}
	s.mu.Unlock()
	if ok {
		s.notify()
	}
}

func (s *Store) notify() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

func hasContainer(pod *corev1.Pod, name string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return true
		}
	}
	return false
}
