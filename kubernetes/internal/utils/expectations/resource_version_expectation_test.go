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
	"testing"

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
