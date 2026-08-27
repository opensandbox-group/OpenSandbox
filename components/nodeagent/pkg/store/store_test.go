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

package store

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStoreFiltersAndRetainsIdentity(t *testing.T) {
	s := New(fake.NewSimpleClientset(), "node-1", "prod-a", "/var/log/pods")
	s.upsert(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "team-a", UID: types.UID("u1"), Labels: map[string]string{SandboxIDLabel: "sb-1"}}, Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: ContainerName}}}})
	s.upsert(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "team-a", UID: types.UID("u2"), Labels: map[string]string{SandboxIDLabel: "sb-2", PoolNameLabel: "pool-a"}}, Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: ContainerName}}}})
	got := s.List()
	if len(got) != 1 || got[0].SandboxID != "sb-1" {
		t.Fatalf("resources=%+v", got)
	}
	s.deleted(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("u1")}})
	resource, ok := s.GetByUID("u1")
	if !ok || !resource.Terminated {
		t.Fatalf("deleted identity was not retained: %+v", resource)
	}
}

func TestStoreStaleOnlyAfterThresholdAndClearsOnRelist(t *testing.T) {
	s := New(fake.NewSimpleClientset(), "node-1", "prod-a", "/var/log/pods")
	s.markWatchFailed()
	if s.Stale(time.Now(), time.Hour) {
		t.Fatal("watch became stale before threshold")
	}
	if !s.Stale(time.Now().Add(2*time.Hour), time.Hour) {
		t.Fatal("watch did not become stale after threshold")
	}
	s.markWatchSuccessful()
	if s.Stale(time.Now().Add(2*time.Hour), time.Hour) {
		t.Fatal("successful relist did not clear stale state")
	}
}

func TestStoreForgetsOnlyTerminatedIdentity(t *testing.T) {
	s := New(fake.NewSimpleClientset(), "node-1", "prod-a", "/var/log/pods")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "team-a", UID: types.UID("u1"), Labels: map[string]string{SandboxIDLabel: "sb-1"}}, Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: ContainerName}}}}
	s.upsert(pod)
	s.Forget("u1")
	if _, found := s.GetByUID("u1"); !found {
		t.Fatal("active identity was forgotten")
	}
	s.deleted(pod)
	s.Forget("u1")
	if _, found := s.GetByUID("u1"); found {
		t.Fatal("terminated identity was retained")
	}
}
