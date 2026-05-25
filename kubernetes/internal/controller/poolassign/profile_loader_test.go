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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileStore_LoadFromConfigMap(t *testing.T) {
	t.Run("valid configmap data", func(t *testing.T) {
		store := NewProfileStore()
		profiles := []Profile{
			{
				Name: "custom",
				Plugins: PluginsSpec{
					Predicate: []string{"image"},
					Score:     []ScoreSpec{{Name: "resbalance", Weight: 50}},
				},
				PluginConf: []PluginConf{
					{Name: "resbalance", Args: map[string]interface{}{"strategy": "MostAllocated"}},
				},
			},
		}
		data, _ := json.Marshal(profiles)
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-profiles"},
			Data:       map[string]string{"profiles": string(data)},
		}

		err := store.LoadFromConfigMap(cm)
		assert.NoError(t, err)

		p := store.GetProfile("custom")
		assert.Equal(t, "custom", p.Name)
		assert.Equal(t, []string{"image"}, p.Plugins.Predicate)
	})

	t.Run("missing profiles key falls back to default", func(t *testing.T) {
		store := NewProfileStore()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-profiles"},
			Data:       map[string]string{},
		}

		err := store.LoadFromConfigMap(cm)
		assert.NoError(t, err)

		p := store.GetProfile(DefaultProfileName)
		assert.Equal(t, DefaultProfile(), p)
	})

	t.Run("invalid JSON falls back to default", func(t *testing.T) {
		store := NewProfileStore()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-profiles"},
			Data:       map[string]string{"profiles": "not-json"},
		}

		err := store.LoadFromConfigMap(cm)
		assert.NoError(t, err)

		p := store.GetProfile(DefaultProfileName)
		assert.Equal(t, DefaultProfile(), p)
	})

	t.Run("empty string falls back to default", func(t *testing.T) {
		store := NewProfileStore()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-profiles"},
			Data:       map[string]string{"profiles": ""},
		}

		err := store.LoadFromConfigMap(cm)
		assert.NoError(t, err)

		p := store.GetProfile(DefaultProfileName)
		assert.Equal(t, DefaultProfile(), p)
	})
}

func TestProfileStore_GetProfile(t *testing.T) {
	t.Run("returns stored profile", func(t *testing.T) {
		store := NewProfileStore()
		_ = store.LoadDefault()

		p := store.GetProfile(DefaultProfileName)
		assert.Equal(t, DefaultProfileName, p.Name)
	})

	t.Run("returns default for unknown name", func(t *testing.T) {
		store := NewProfileStore()
		_ = store.LoadDefault()

		p := store.GetProfile("nonexistent")
		assert.Equal(t, DefaultProfile(), p)
	})

	t.Run("returns default when store is empty", func(t *testing.T) {
		store := NewProfileStore()

		p := store.GetProfile("anything")
		assert.Equal(t, DefaultProfile(), p)
	})
}

func TestProfileStore_LoadDefault(t *testing.T) {
	store := NewProfileStore()
	err := store.LoadDefault()
	assert.NoError(t, err)

	p := store.GetProfile(DefaultProfileName)
	assert.Equal(t, DefaultProfile(), p)
}

func TestProfileStore_SetupWithManager_EmptyNamespace(t *testing.T) {
	store := NewProfileStore()
	err := store.SetupWithManager(nil, "")
	assert.NoError(t, err)
	assert.Empty(t, store.namespace)
}

func TestProfileStore_WatchConfigMap(t *testing.T) {
	const ns = "test-ns"

	fakeClient := fake.NewSimpleClientset()
	store := NewProfileStore()
	store.namespace = ns

	factory, err := store.setupInformer(fakeClient)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	profiles := []Profile{
		{
			Name: "custom",
			Plugins: PluginsSpec{
				Predicate: []string{"image"},
				Score:     []ScoreSpec{{Name: "resbalance", Weight: 50}},
			},
		},
	}
	data, _ := json.Marshal(profiles)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ProfileConfigMapName, Namespace: ns},
		Data:       map[string]string{"profiles": string(data)},
	}

	_, err = fakeClient.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		p := store.GetProfile("custom")
		return p != nil && p.Name == "custom"
	}, 2*time.Second, 100*time.Millisecond, "profile should be loaded after ConfigMap creation")

	updatedProfiles := []Profile{
		{
			Name: "updated",
			Plugins: PluginsSpec{
				Predicate: []string{"labelselector"},
				Score:     []ScoreSpec{{Name: "resbalance", Weight: 100}},
			},
		},
	}
	updatedData, _ := json.Marshal(updatedProfiles)
	cm.Data["profiles"] = string(updatedData)
	_, err = fakeClient.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		p := store.GetProfile("updated")
		return p != nil && p.Name == "updated"
	}, 2*time.Second, 100*time.Millisecond, "profile should be updated after ConfigMap update")
}
