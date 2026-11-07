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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils/expectations"
)

func TestForgetDeletedBatchSandboxDropsStatusExpectation(t *testing.T) {
	sandbox := func(name, resourceVersion string) *sandboxv1alpha1.BatchSandbox {
		return &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            name,
			UID:             types.UID("uid-" + name),
			ResourceVersion: resourceVersion,
		}}
	}
	r := &BatchSandboxReconciler{StatusRVExpectation: expectations.NewResourceVersionExpectation()}
	r.StatusRVExpectation.Expect(sandbox("deleted", "5"))
	r.StatusRVExpectation.Expect(sandbox("live", "5"))

	r.forgetDeletedBatchSandbox().Delete(context.Background(), event.DeleteEvent{Object: sandbox("deleted", "5")}, nil)

	if satisfied, _ := r.StatusRVExpectation.IsSatisfied(sandbox("deleted", "4")); !satisfied {
		t.Fatal("the deleted BatchSandbox's status expectation was kept")
	}
	if satisfied, _ := r.StatusRVExpectation.IsSatisfied(sandbox("live", "4")); satisfied {
		t.Fatal("another BatchSandbox's status expectation was dropped")
	}
}
