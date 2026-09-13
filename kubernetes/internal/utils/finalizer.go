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

package utils

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type finalizerOpType string

const (
	AddFinalizerOpType    finalizerOpType = "Add"
	RemoveFinalizerOpType finalizerOpType = "Remove"
)

func UpdateFinalizer(c client.Client, object client.Object, op finalizerOpType, finalizer string) error {
	switch op {
	case AddFinalizerOpType, RemoveFinalizerOpType:
	default:
		return errors.New("UpdateFinalizer Func 'op' parameter must be 'Add' or 'Remove'")
	}

	key := client.ObjectKeyFromObject(object)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fetchedObject := object.DeepCopyObject().(client.Object)
		getErr := c.Get(context.TODO(), key, fetchedObject)
		if getErr != nil {
			return getErr
		}
		finalizers := fetchedObject.GetFinalizers()
		switch op {
		case AddFinalizerOpType:
			if controllerutil.ContainsFinalizer(fetchedObject, finalizer) {
				return nil
			}
			finalizers = append(finalizers, finalizer)
		case RemoveFinalizerOpType:
			finalizerSet := sets.NewString(finalizers...)
			if !finalizerSet.Has(finalizer) {
				return nil
			}
			finalizers = finalizerSet.Delete(finalizer).List()
		}
		fetchedObject.SetFinalizers(finalizers)
		return c.Update(context.TODO(), fetchedObject)
	})
}
