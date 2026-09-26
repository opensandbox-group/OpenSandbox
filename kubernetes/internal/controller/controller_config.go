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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// FeatureConfigMapName is the feature ConfigMap in the controller's own
	// namespace.
	FeatureConfigMapName = "feature-flags"

	featureConfigKeyPodRecoveryStuckThreshold = "pod-recovery-stuck-threshold"
	featureConfigKeyPodRecoveryMaxAttempts    = "pod-recovery-max-attempts"
)

var featureConfigLog = logf.Log.WithName("feature-config")

// FeatureConfig holds configuration loaded from the feature ConfigMap.
// Missing or invalid entries fall back to built-in defaults at read time.
type FeatureConfig struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewFeatureConfig() *FeatureConfig {
	return &FeatureConfig{data: map[string]string{}}
}

// Load replaces the in-memory configuration.
func (c *FeatureConfig) Load(data map[string]string) {
	loaded := make(map[string]string, len(data))
	for k, v := range data {
		loaded[k] = strings.TrimSpace(v)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = loaded
}

func (c *FeatureConfig) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *FeatureConfig) duration(key string, fallback time.Duration) time.Duration {
	if raw, ok := c.get(key); ok {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func (c *FeatureConfig) positiveInt(key string, fallback int) int {
	if raw, ok := c.get(key); ok {
		if n, err := parsePositiveInt(raw); err == nil {
			return n
		}
	}
	return fallback
}

func parsePositiveInt(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid positive integer %q", raw)
	}
	return n, nil
}

// SetupWithManager watches the feature ConfigMap and reloads the in-memory
// configuration on changes. An empty namespace skips the watch.
func (c *FeatureConfig) SetupWithManager(mgr manager.Manager, namespace string) error {
	if namespace == "" {
		featureConfigLog.Info("namespace is empty, skipping feature config ConfigMap watch")
		return nil
	}

	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 30*time.Second,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", FeatureConfigMapName).String()
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.load(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			c.load(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.Load(nil)
		},
	})
	if err != nil {
		return err
	}

	return mgr.Add(&featureConfigRunnable{factory: factory})
}

func (c *FeatureConfig) load(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	featureConfigLog.Info("Loaded feature config from ConfigMap", "namespace", cm.Namespace, "name", cm.Name, "data", cm.Data)
	c.Load(cm.Data)
}

type featureConfigRunnable struct {
	factory featureConfigInformerFactory
}

func (r *featureConfigRunnable) Start(ctx context.Context) error {
	r.factory.Start(ctx.Done())
	r.factory.WaitForCacheSync(ctx.Done())
	return nil
}

type featureConfigInformerFactory interface {
	Start(stopCh <-chan struct{})
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool
}
