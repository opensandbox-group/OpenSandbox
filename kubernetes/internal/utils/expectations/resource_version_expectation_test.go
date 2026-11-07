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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResourceVersionExpectation(t *testing.T) {
	cases := []struct {
		expect      *v1.Pod
		observe     *v1.Pod
		isSatisfied *v1.Pod
		result      bool
	}{
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			result:      false,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			result:      true,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			result:      true,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3"}},
			result:      true,
		},
	}

	for i, testCase := range cases {
		c := NewResourceVersionExpectation()
		c.Expect(testCase.expect)
		c.Observe(testCase.observe)
		got, _ := c.IsSatisfied(testCase.isSatisfied)
		if got != testCase.result {
			t.Fatalf("#%d expected %v, got %v", i, testCase.result, got)
		}
	}
}

func TestResourceVersionExpectationRecreatedObject(t *testing.T) {
	c := NewResourceVersionExpectation().(*realResourceVersionExpectation)
	old := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sandbox", UID: "old", ResourceVersion: "100"}}
	c.Expect(old)
	staleOld := old.DeepCopy()
	staleOld.ResourceVersion = "99"
	satisfied, _ := c.IsSatisfied(staleOld)
	require.False(t, satisfied)

	replacement := old.DeepCopy()
	replacement.UID = "replacement"
	replacement.ResourceVersion = "2"
	satisfied, _ = c.IsSatisfied(replacement)
	require.True(t, satisfied, "a new UID must not inherit the old expectation")
	c.Expect(replacement)
	require.Len(t, c.objectVersions, 1, "reusing a name must replace the obsolete UID entry")
	key := types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}
	require.True(t, c.objectVersions[key].firstUnsatisfiedTimestamp.IsZero())
	require.Equal(t, "2", c.objectVersions[key].version)

	// Delayed operations for the old UID cannot clear the replacement's entry.
	c.Observe(old)
	c.Delete(old)
	satisfied, _ = c.IsSatisfied(old)
	require.True(t, satisfied)
	replacement.ResourceVersion = "1"
	satisfied, _ = c.IsSatisfied(replacement)
	require.False(t, satisfied)
	replacement.ResourceVersion = "2"
	c.Observe(replacement)
	require.Empty(t, c.objectVersions)
}

func TestResourceVersionExpectationDeleteByKey(t *testing.T) {
	c := NewResourceVersionExpectation().(*realResourceVersionExpectation)
	for _, namespace := range []string{"one", "two"} {
		c.Expect(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "sandbox", UID: types.UID(namespace), ResourceVersion: "2"}})
	}
	require.Len(t, c.objectVersions, 2)
	key := types.NamespacedName{Namespace: "one", Name: "sandbox"}
	c.DeleteByKey(key)
	c.DeleteByKey(key) // A repeated NotFound reconcile is harmless.
	require.Len(t, c.objectVersions, 1)
	other := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "two", Name: "sandbox", UID: "two", ResourceVersion: "1"}}
	satisfied, _ := c.IsSatisfied(other)
	require.False(t, satisfied)
	c.Delete(other)
	require.Empty(t, c.objectVersions)
}

func TestResourceVersionExpectationDeletionChurn(t *testing.T) {
	c := NewResourceVersionExpectation().(*realResourceVersionExpectation)
	for i := 0; i < 2500; i++ {
		name := fmt.Sprintf("sandbox-%d", i)
		c.Expect(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(name), ResourceVersion: "2"}})
	}
	require.Len(t, c.objectVersions, 2500)
	for i := 0; i < 2500; i++ {
		c.DeleteByKey(types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("sandbox-%d", i)})
	}
	require.Empty(t, c.objectVersions, "deleted objects must not accumulate over the controller lifetime")
}
