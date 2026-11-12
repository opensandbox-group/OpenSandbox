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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

var _ = Describe("Pool capacitySpec validation", func() {
	newPool := func(capacity sandboxv1alpha1.CapacitySpec) *sandboxv1alpha1.Pool {
		return &sandboxv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pool-capacity-validation-" + rand.String(8),
				Namespace: "default",
			},
			Spec: sandboxv1alpha1.PoolSpec{
				Template: &v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{Name: "main", Image: "example.com"}},
					},
				},
				CapacitySpec: capacity,
			},
		}
	}

	It("rejects a Pool whose bufferMin exceeds bufferMax", func() {
		ctx := context.Background()
		pool := newPool(sandboxv1alpha1.CapacitySpec{PoolMin: 0, PoolMax: 10, BufferMin: 3, BufferMax: 1})
		err := k8sClient.Create(ctx, pool)
		if err == nil {
			_ = k8sClient.Delete(ctx, pool)
		}
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
		Expect(err.Error()).To(ContainSubstring("bufferMin must not exceed bufferMax"))
	})

	It("rejects an update that raises bufferMin above bufferMax", func() {
		ctx := context.Background()
		pool := newPool(sandboxv1alpha1.CapacitySpec{PoolMin: 0, PoolMax: 0, BufferMin: 1, BufferMax: 1})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pool) })

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &sandboxv1alpha1.Pool{}
			if err := k8sClient.Get(ctx, kclient.ObjectKeyFromObject(pool), latest); err != nil {
				return err
			}
			latest.Spec.CapacitySpec.BufferMin = 2
			return k8sClient.Update(ctx, latest)
		})
		Expect(errors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	})

	It("accepts a Pool whose bufferMin equals bufferMax", func() {
		ctx := context.Background()
		pool := newPool(sandboxv1alpha1.CapacitySpec{PoolMin: 0, PoolMax: 0, BufferMin: 2, BufferMax: 2})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
	})
})
