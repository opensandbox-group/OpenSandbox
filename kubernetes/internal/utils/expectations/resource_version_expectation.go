// Copyright 2025 The OpenSandbox Authors
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

package expectations

import (
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type ResourceVersionExpectation interface {
	Expect(obj metav1.Object)
	Observe(obj metav1.Object)
	IsSatisfied(obj metav1.Object) (bool, time.Duration)
	Delete(obj metav1.Object)
	// DeleteByKey clears an expectation after reconciliation observes NotFound.
	DeleteByKey(key types.NamespacedName)
}

func NewResourceVersionExpectation() ResourceVersionExpectation {
	return &realResourceVersionExpectation{objectVersions: make(map[types.NamespacedName]*objectCacheVersions, 100)}
}

type realResourceVersionExpectation struct {
	sync.Mutex
	// Key by namespace/name so a NotFound reconcile can reclaim the entry
	// without an object UID. The stored UID still fences recreated objects.
	objectVersions map[types.NamespacedName]*objectCacheVersions
}

type objectCacheVersions struct {
	uid                       types.UID
	version                   string
	firstUnsatisfiedTimestamp time.Time
}

func (r *realResourceVersionExpectation) Expect(obj metav1.Object) {
	r.Lock()
	defer r.Unlock()

	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	expectations := r.objectVersions[key]
	if expectations == nil || expectations.uid != obj.GetUID() {
		r.objectVersions[key] = &objectCacheVersions{uid: obj.GetUID()}
	}
	if isResourceVersionNewer(r.objectVersions[key].version, obj.GetResourceVersion()) {
		r.objectVersions[key].version = obj.GetResourceVersion()
	}
}

func (r *realResourceVersionExpectation) Observe(obj metav1.Object) {
	r.Lock()
	defer r.Unlock()

	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	expectations := r.objectVersions[key]
	if expectations == nil || expectations.uid != obj.GetUID() {
		return
	}
	if isResourceVersionNewer(r.objectVersions[key].version, obj.GetResourceVersion()) {
		delete(r.objectVersions, key)
	}
}

func (r *realResourceVersionExpectation) IsSatisfied(obj metav1.Object) (bool, time.Duration) {
	r.Lock()
	defer r.Unlock()

	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	expectations := r.objectVersions[key]
	if expectations == nil || expectations.uid != obj.GetUID() {
		return true, 0
	}

	if isResourceVersionNewer(r.objectVersions[key].version, obj.GetResourceVersion()) {
		delete(r.objectVersions, key)
	}
	_, existing := r.objectVersions[key]
	if existing {
		if r.objectVersions[key].firstUnsatisfiedTimestamp.IsZero() {
			r.objectVersions[key].firstUnsatisfiedTimestamp = time.Now()
		}

		return false, time.Since(r.objectVersions[key].firstUnsatisfiedTimestamp)
	}

	return !existing, 0
}

func (r *realResourceVersionExpectation) Delete(obj metav1.Object) {
	r.Lock()
	defer r.Unlock()
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if expectation := r.objectVersions[key]; expectation != nil && expectation.uid == obj.GetUID() {
		delete(r.objectVersions, key)
	}
}

func (r *realResourceVersionExpectation) DeleteByKey(key types.NamespacedName) {
	r.Lock()
	defer r.Unlock()
	delete(r.objectVersions, key)
}

func isResourceVersionNewer(old, new string) bool {
	if len(old) == 0 {
		return true
	}

	oldCount, err := strconv.ParseUint(old, 10, 64)
	if err != nil {
		return true
	}

	newCount, err := strconv.ParseUint(new, 10, 64)
	if err != nil {
		return false
	}

	return newCount >= oldCount
}
