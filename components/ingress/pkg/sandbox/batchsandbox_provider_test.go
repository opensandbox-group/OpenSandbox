// Copyright 2025 Alibaba Group Holding Ltd.
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

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	fakeclientset "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/clientset/versioned/fake"
	informers "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/client/informers/externalversions"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

// Note: Integration tests with real informers are in e2e tests
// Unit tests here focus on provider behavior

// TestBatchSandboxProvider_WithFakeInformer tests the provider using fake clientset and informer
func TestBatchSandboxProvider_WithFakeInformer(t *testing.T) {
	namespace := "test-namespace"

	// Create a ready BatchSandbox with valid endpoints
	readyBatchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ready-sandbox",
			Namespace: namespace,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `["10.0.0.1", "10.0.0.2"]`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(2)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 2,
			Ready:    2,
		},
	}

	// Create a not ready BatchSandbox
	notReadyBatchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-ready-sandbox",
			Namespace: namespace,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 1,
			Ready:    0,
		},
	}

	// Create fake clientset with test objects
	fakeClient := fakeclientset.NewSimpleClientset(readyBatchSandbox, notReadyBatchSandbox)

	// Create informer factory
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	// Create provider
	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	// Start informer and wait for cache sync
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	assert.NoError(t, err, "Provider should start successfully")

	// Manually add objects to informer cache (fake clientset doesn't auto-populate informer)
	err = batchSandboxInformer.Informer().GetStore().Add(readyBatchSandbox)
	assert.NoError(t, err)
	err = batchSandboxInformer.Informer().GetStore().Add(notReadyBatchSandbox)
	assert.NoError(t, err)

	// Test 1: Get endpoint from ready sandbox
	t.Run("GetEndpoint from ready sandbox", func(t *testing.T) {
		endpoint, err := provider.GetEndpoint("ready-sandbox")
		assert.NoError(t, err)
		assert.Equal(t, "10.0.0.1", endpoint.Endpoint, "Should return first endpoint IP")
		assert.Equal(t, "", endpoint.SecureAccessToken)
	})

	// Test 2: Get endpoint from not ready sandbox
	t.Run("GetEndpoint from not ready sandbox", func(t *testing.T) {
		_, err := provider.GetEndpoint("not-ready-sandbox")
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrSandboxNotReady), "Should return ErrSandboxNotReady")
		assert.Contains(t, err.Error(), "not ready")
	})

	// Test 3: Get endpoint from non-existent sandbox
	t.Run("GetEndpoint from non-existent sandbox", func(t *testing.T) {
		_, err := provider.GetEndpoint("non-existent")
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrSandboxNotFound), "Should return ErrSandboxNotFound")
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("GetEndpoint with non-empty access-token", func(t *testing.T) {
		secureSB := &sandboxv1alpha1.BatchSandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "secure-sb",
				Namespace: namespace,
				Annotations: map[string]string{
					AnnotationAccessToken:     "opaque",
					utils.AnnotationEndpoints: `["10.0.0.1"]`,
				},
			},
			Spec: sandboxv1alpha1.BatchSandboxSpec{
				Replicas: ptr(int32(1)),
			},
			Status: sandboxv1alpha1.BatchSandboxStatus{
				Replicas: 1,
				Ready:    1,
			},
		}
		err := batchSandboxInformer.Informer().GetStore().Add(secureSB)
		assert.NoError(t, err)
		info, err := provider.GetEndpoint("secure-sb")
		assert.NoError(t, err)
		assert.Equal(t, "10.0.0.1", info.Endpoint)
		assert.Equal(t, "opaque", info.SecureAccessToken)
	})
}

// TestBatchSandboxProvider_MissingAnnotation tests sandbox without endpoints annotation
func TestBatchSandboxProvider_MissingAnnotation(t *testing.T) {
	namespace := "test-namespace"

	// Create BatchSandbox without endpoints annotation
	batchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-annotation-sandbox",
			Namespace: namespace,
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 1,
			Ready:    1,
		},
	}

	fakeClient := fakeclientset.NewSimpleClientset(batchSandbox)
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	assert.NoError(t, err)

	// Manually add object to informer cache
	err = batchSandboxInformer.Informer().GetStore().Add(batchSandbox)
	assert.NoError(t, err)

	_, err = provider.GetEndpoint("no-annotation-sandbox")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrSandboxNotReady), "Should return ErrSandboxNotReady")
	assert.Contains(t, err.Error(), "has no annotations")
}

// TestBatchSandboxProvider_InvalidAnnotation tests sandbox with invalid annotation format
func TestBatchSandboxProvider_InvalidAnnotation(t *testing.T) {
	namespace := "test-namespace"

	batchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-annotation-sandbox",
			Namespace: namespace,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `invalid-json`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 1,
			Ready:    1,
		},
	}

	fakeClient := fakeclientset.NewSimpleClientset(batchSandbox)
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	assert.NoError(t, err)

	// Manually add object to informer cache
	err = batchSandboxInformer.Informer().GetStore().Add(batchSandbox)
	assert.NoError(t, err)

	_, err = provider.GetEndpoint("invalid-annotation-sandbox")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrSandboxNotReady), "Should return ErrSandboxNotReady")
	assert.Contains(t, err.Error(), "failed to parse")
}

// TestBatchSandboxProvider_DynamicUpdate tests adding object after informer starts
func TestBatchSandboxProvider_DynamicUpdate(t *testing.T) {
	namespace := "test-namespace"

	fakeClient := fakeclientset.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	assert.NoError(t, err)

	// Initially no sandbox exists
	_, err = provider.GetEndpoint("dynamic-sandbox")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrSandboxNotFound), "Should return ErrSandboxNotFound")
	assert.Contains(t, err.Error(), "not found")

	// Add a new BatchSandbox
	newBatchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dynamic-sandbox",
			Namespace: namespace,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `["10.0.0.100"]`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 1,
			Ready:    1,
		},
	}

	_, err = fakeClient.SandboxV1alpha1().BatchSandboxes(namespace).Create(
		context.Background(), newBatchSandbox, metav1.CreateOptions{})
	assert.NoError(t, err)

	// Wait for informer to pick up the change
	assert.Eventually(t, func() bool {
		endpoint, err := provider.GetEndpoint("dynamic-sandbox")
		return err == nil && endpoint.Endpoint == "10.0.0.100"
	}, 3*time.Second, 100*time.Millisecond, "Informer should eventually sync the new object")
}

// TestBatchSandboxProvider_StartCacheSyncFailure tests cache sync timeout
func TestBatchSandboxProvider_StartCacheSyncFailure(t *testing.T) {
	namespace := "test-namespace"

	fakeClient := fakeclientset.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	// Create a context that expires immediately
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Wait for context to expire
	time.Sleep(10 * time.Millisecond)

	err := provider.Start(ctx)
	assert.Error(t, err, "Should fail when cache sync times out")
	assert.Contains(t, err.Error(), "failed to sync")
}

// TestBatchSandboxProvider_GetEndpointNonNotFoundError tests non-IsNotFound K8s errors
func TestBatchSandboxProvider_GetEndpointNonNotFoundError(t *testing.T) {
	namespace := "test-namespace"

	// Create a sandbox with Ready status but missing endpoint annotation
	batchSandbox := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-endpoint-sandbox",
			Namespace: namespace,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `["10.0.0.1"]`,
			},
		},
		Spec: sandboxv1alpha1.BatchSandboxSpec{
			Replicas: ptr(int32(1)),
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{
			Replicas: 1,
			Ready:    1,
		},
	}

	fakeClient := fakeclientset.NewSimpleClientset(batchSandbox)
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(namespace),
	)

	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()

	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	assert.NoError(t, err)

	// Manually add object to informer cache
	err = batchSandboxInformer.Informer().GetStore().Add(batchSandbox)
	assert.NoError(t, err)

	// Should successfully get endpoint
	endpoint, err := provider.GetEndpoint("missing-endpoint-sandbox")
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.1", endpoint.Endpoint)
}

func TestBatchSandboxProvider_GetEndpoint_AmbiguousAcrossNamespaces(t *testing.T) {
	namespaceA := "ns-a"
	namespaceB := "ns-b"
	sandboxName := "shared-id"

	first := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: namespaceA,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `["10.0.0.1"]`,
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{Replicas: 1, Ready: 1},
	}
	second := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: namespaceB,
			Annotations: map[string]string{
				utils.AnnotationEndpoints: `["10.0.0.2"]`,
			},
		},
		Status: sandboxv1alpha1.BatchSandboxStatus{Replicas: 1, Ready: 1},
	}

	fakeClient := fakeclientset.NewSimpleClientset(first, second)
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		fakeClient,
		time.Second*30,
		informers.WithNamespace(metav1.NamespaceAll),
	)
	batchSandboxInformer := informerFactory.Sandbox().V1alpha1().BatchSandboxes()
	provider := &BatchSandboxProvider{
		informerFactory: informerFactory,
		lister:          batchSandboxInformer.Lister(),
		informerSynced:  batchSandboxInformer.Informer().HasSynced,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := provider.Start(ctx)
	assert.NoError(t, err)
	assert.NoError(t, batchSandboxInformer.Informer().GetStore().Add(first))
	assert.NoError(t, batchSandboxInformer.Informer().GetStore().Add(second))

	_, err = provider.GetEndpoint(sandboxName)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "ambiguous sandbox id"))
}

// ptr is a helper function to create int32 pointer
func ptr(i int32) *int32 {
	return &i
}
