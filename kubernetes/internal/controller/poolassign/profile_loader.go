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

package assign

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const ProfileConfigMapName = "pool-assign-profiles"

var log = logf.Log.WithName("profile-store")

type ProfileStore struct {
	mu        sync.RWMutex
	profiles  map[string]*Profile
	client    client.Client
	namespace string
}

func NewProfileStore() *ProfileStore {
	return &ProfileStore{
		profiles: make(map[string]*Profile),
	}
}

func (s *ProfileStore) SetupWithManager(mgr manager.Manager, namespace string) error {
	if namespace == "" {
		log.Info("namespace is empty, skipping assign profiles ConfigMap watch")
		return nil
	}
	s.client = mgr.GetClient()
	s.namespace = namespace

	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}

	factory, err := s.setupInformer(kubeClient)
	if err != nil {
		return err
	}

	if err := mgr.Add(&profileStoreRunnable{factory: factory}); err != nil {
		return err
	}

	return nil
}

func (s *ProfileStore) setupInformer(kubeClient kubernetes.Interface) (sharedInformerFactory, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 30*time.Second,
		informers.WithNamespace(s.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", ProfileConfigMapName).String()
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()

	_, err := informer.AddEventHandler(s.buildEventHandler())
	if err != nil {
		return nil, err
	}

	return factory, nil
}

func (s *ProfileStore) buildEventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return
			}
			log.Info("detected assign profiles ConfigMap", "namespace", cm.Namespace, "name", cm.Name, "resourceVersion", cm.ResourceVersion, "data", cm.Data)
			if err := s.LoadFromConfigMap(cm); err != nil {
				log.Error(err, "failed to load assign profiles from ConfigMap", "namespace", cm.Namespace, "name", cm.Name)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok {
				return
			}
			log.Info("assign profiles ConfigMap updated", "namespace", cm.Namespace, "name", cm.Name, "resourceVersion", cm.ResourceVersion, "data", cm.Data)
			if err := s.LoadFromConfigMap(cm); err != nil {
				log.Error(err, "failed to load assign profiles from ConfigMap", "namespace", cm.Namespace, "name", cm.Name)
			}
		},
	}
}

type sharedInformerFactory interface {
	Start(stopCh <-chan struct{})
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool
}

type profileStoreRunnable struct {
	factory sharedInformerFactory
}

func (r *profileStoreRunnable) Start(ctx context.Context) error {
	r.factory.Start(ctx.Done())
	r.factory.WaitForCacheSync(ctx.Done())
	return nil
}

func (s *ProfileStore) LoadFromConfigMap(cm *corev1.ConfigMap) error {
	raw, ok := cm.Data["profiles"]
	if !ok || raw == "" {
		return s.LoadDefault()
	}
	var profiles []Profile
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil {
		return s.LoadDefault()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = make(map[string]*Profile, len(profiles))
	for i := range profiles {
		s.profiles[profiles[i].Name] = &profiles[i]
	}
	return nil
}

func (s *ProfileStore) GetProfile(name string) *Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.profiles[name]; ok {
		return p
	}
	return DefaultProfile()
}

func (s *ProfileStore) LoadDefault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := DefaultProfile()
	s.profiles = map[string]*Profile{d.Name: d}
	return nil
}
