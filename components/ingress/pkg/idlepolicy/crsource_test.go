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

package idlepolicy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// fakeDynamic satisfies dynamic.Interface; the vendored dynamicfake
// indexes objects by naive pluralization ("sandboxs") and cannot serve the
// real "sandboxes" resource.
type fakeDynamic struct {
	objects map[string]*unstructured.Unstructured
	getErrs map[string]error
	gets    map[string]int
	mu      sync.Mutex
	delay   time.Duration
}

func newFakeDynamic() *fakeDynamic {
	return &fakeDynamic{
		objects: map[string]*unstructured.Unstructured{},
		getErrs: map[string]error{},
		gets:    map[string]int{},
	}
}

func (f *fakeDynamic) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return f
}

func (f *fakeDynamic) Namespace(string) dynamic.ResourceInterface { return f }

func (f *fakeDynamic) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	f.gets[name]++
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err, ok := f.getErrs[name]; ok {
		return nil, err
	}
	if obj, ok := f.objects[name]; ok {
		return obj, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeDynamic) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	return errors.New("unsupported")
}

func (f *fakeDynamic) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	return errors.New("unsupported")
}

func (f *fakeDynamic) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

func (f *fakeDynamic) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return nil, errors.New("unsupported")
}

var (
	_ dynamic.Interface                      = (*fakeDynamic)(nil)
	_ dynamic.NamespaceableResourceInterface = (*fakeDynamic)(nil)
	_ dynamic.ResourceInterface              = (*fakeDynamic)(nil)
)

func annotatedSandbox(t *testing.T, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(SandboxGVR.GroupVersion().String())
	obj.SetKind("Sandbox")
	obj.SetNamespace("tenant-a")
	obj.SetName("sb-1")
	obj.SetAnnotations(annotations)
	return obj
}

func TestCRPolicySourceReadsAnnotation(t *testing.T) {
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	client.objects["sb-denied"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"wakeOnAccess":false}`})
	client.objects["sb-unpoliced"] = annotatedSandbox(t, nil)
	source := NewCRPolicySource(context.Background(), client, time.Minute)

	require.True(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-denied"))
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-unpoliced"))
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "missing"))
}

func TestCRPolicySourceCachesUntilTTLAndFailsSafeOnAPIFailure(t *testing.T) {
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	source := NewCRPolicySource(context.Background(), client, time.Minute)

	require.True(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))

	client.getErrs["sb-1"] = errors.New("api server offline")
	require.True(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"), "cache TTL keeps the last answer")
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-2"))
}

func TestCRPolicySourceDoesNotCacheErrors(t *testing.T) {
	// A transient API failure must not become a sticky denial for the TTL.
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	source := NewCRPolicySource(context.Background(), client, time.Minute)
	source.failureTTL = 0

	client.getErrs["sb-1"] = errors.New("flaky")
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))

	delete(client.getErrs, "sb-1")
	require.True(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"), "the failed read must not have been cached")
}

func TestLeaderCancelDoesNotPoisonJoiners(t *testing.T) {
	// The shared read is detached from the leader's context: the leader
	// disconnecting mid-GET must not fail the joiners waiting on it.
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	client.delay = 100 * time.Millisecond
	source := NewCRPolicySource(context.Background(), client, time.Minute)

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		if !source.WakeAllowed(leaderCtx, "tenant-a", "sb-1") {
			leaderErr <- errors.New("leader denied")
		} else {
			leaderErr <- nil
		}
	}()
	time.Sleep(10 * time.Millisecond) // the leader has registered the inflight read
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel() // leader disconnects mid-GET
	}()

	joinerErr := make(chan error, 2)
	for range 2 {
		go func() {
			if source.WakeAllowed(context.Background(), "tenant-a", "sb-1") {
				joinerErr <- nil
			} else {
				joinerErr <- errors.New("joiner denied")
			}
		}()
	}
	require.NoError(t, <-leaderErr)
	require.NoError(t, <-joinerErr)
	require.NoError(t, <-joinerErr)
	require.Equal(t, 1, client.gets["sb-1"])
}

func TestFailureBackoffLimitsRetries(t *testing.T) {
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	client.getErrs["sb-1"] = errors.New("api server offline")
	source := NewCRPolicySource(context.Background(), client, time.Minute)

	base := time.Now()
	clock := base
	source.now = func() time.Time { return clock }

	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))
	require.Equal(t, 1, client.gets["sb-1"], "the failure backoff must rate-limit retries")

	clock = base.Add(failureBackoff + time.Second)
	require.False(t, source.WakeAllowed(context.Background(), "tenant-a", "sb-1"))
	require.Equal(t, 2, client.gets["sb-1"], "after the backoff window a retry is allowed")
}

func TestCRPolicySourceCollapsesColdCacheReads(t *testing.T) {
	client := newFakeDynamic()
	client.objects["sb-1"] = annotatedSandbox(t, map[string]string{AnnotationKey: `{"idleTimeoutSeconds":300,"onIdle":"pause"}`})
	source := NewCRPolicySource(context.Background(), client, time.Minute)

	var wg sync.WaitGroup
	results := make([]bool, 32)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = source.WakeAllowed(context.Background(), "tenant-a", "sb-1")
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		require.True(t, r)
	}
	require.Equal(t, 1, client.gets["sb-1"], "concurrent cold-cache reads must collapse to one GET")
}
