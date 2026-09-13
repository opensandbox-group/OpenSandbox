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

package expectations

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

// scaleAction is the action of scale, like create and delete.
type scaleAction string

const (
	// Create action
	Create scaleAction = "create"
	// Delete action
	Delete scaleAction = "delete"
)

// scaleExpectations is an interface that allows users to set and wait on expectations of pods scale.
type scaleExpectations interface {
	ExpectScale(controllerKey string, action scaleAction, name string)
	ObserveScale(controllerKey string, action scaleAction, name string)
	SatisfiedExpectations(controllerKey string) (bool, time.Duration, map[scaleAction][]string)
	DeleteExpectations(controllerKey string)
	GetExpectations(controllerKey string) map[scaleAction]sets.String
}

// NewScaleExpectations returns a common scaleExpectations.
func NewScaleExpectations() scaleExpectations {
	return &realScaleExpectations{
		controllerCache: make(map[string]*realControllerScaleExpectations),
	}
}

type realScaleExpectations struct {
	sync.Mutex
	// key: parent key, workload namespace/name
	controllerCache map[string]*realControllerScaleExpectations
}

type realControllerScaleExpectations struct {
	// item: name for this object
	objsCache                 map[scaleAction]sets.String
	firstUnsatisfiedTimestamp time.Time
}

func (r *realScaleExpectations) GetExpectations(controllerKey string) map[scaleAction]sets.String {
	r.Lock()
	defer r.Unlock()

	expectations := r.controllerCache[controllerKey]
	if expectations == nil {
		return nil
	}

	res := make(map[scaleAction]sets.String, len(expectations.objsCache))
	for k, v := range expectations.objsCache {
		res[k] = sets.NewString(v.List()...)
	}

	return res
}

func (r *realScaleExpectations) ExpectScale(controllerKey string, action scaleAction, name string) {
	r.Lock()
	defer r.Unlock()

	expectations := r.controllerCache[controllerKey]
	if expectations == nil {
		expectations = &realControllerScaleExpectations{
			objsCache: make(map[scaleAction]sets.String),
		}
		r.controllerCache[controllerKey] = expectations
	}

	if s := expectations.objsCache[action]; s != nil {
		s.Insert(name)
	} else {
		expectations.objsCache[action] = sets.NewString(name)
	}
}

func (r *realScaleExpectations) ObserveScale(controllerKey string, action scaleAction, name string) {
	r.Lock()
	defer r.Unlock()

	expectations := r.controllerCache[controllerKey]
	if expectations == nil {
		return
	}

	s := expectations.objsCache[action]
	if s == nil {
		return
	}
	s.Delete(name)

	for _, s := range expectations.objsCache {
		if s.Len() > 0 {
			return
		}
	}
	delete(r.controllerCache, controllerKey)
}

func (r *realScaleExpectations) SatisfiedExpectations(controllerKey string) (bool, time.Duration, map[scaleAction][]string) {
	r.Lock()
	defer r.Unlock()

	expectations := r.controllerCache[controllerKey]
	if expectations == nil {
		return true, 0, nil
	}

	for a, s := range expectations.objsCache {
		if s.Len() > 0 {
			if expectations.firstUnsatisfiedTimestamp.IsZero() {
				expectations.firstUnsatisfiedTimestamp = time.Now()
			}
			return false, time.Since(expectations.firstUnsatisfiedTimestamp), map[scaleAction][]string{a: s.List()}
		}
	}

	delete(r.controllerCache, controllerKey)
	return true, 0, nil
}

func (r *realScaleExpectations) DeleteExpectations(controllerKey string) {
	r.Lock()
	defer r.Unlock()
	delete(r.controllerCache, controllerKey)
}
