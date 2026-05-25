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

package policy

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkEvaluateLinearMiss benchmarks the linear evaluation of a domain that is not present in the policy.
//
// goos: darwin
// goarch: arm64
// pkg: github.com/alibaba/opensandbox/egress/pkg/policy
// cpu: Apple M2 Pro
// BenchmarkEvaluateLinearMiss
// BenchmarkEvaluateLinearMiss/rules_500
// BenchmarkEvaluateLinearMiss/rules_500-10         	   54367	     21454 ns/op	       0 B/op	       0 allocs/op
// BenchmarkEvaluateLinearMiss/rules_1000
// BenchmarkEvaluateLinearMiss/rules_1000-10        	   27912	     42489 ns/op	       0 B/op	       0 allocs/op
// BenchmarkEvaluateLinearMiss/rules_10000
// BenchmarkEvaluateLinearMiss/rules_10000-10       	    2684	    446589 ns/op	       0 B/op	       0 allocs/op
func BenchmarkEvaluateLinearMiss(b *testing.B) {
	for _, ruleCount := range []int{500, 1000, 10000} {
		b.Run(fmt.Sprintf("rules_%d", ruleCount), func(b *testing.B) {
			p := buildDomainOnlyPolicy(ruleCount, false)
			query := "not-found.example.com."

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = p.evaluateLinear(strings.TrimSuffix(strings.ToLower(query), "."))
			}
		})
	}
}

// BenchmarkEvaluateCompiledIndexMiss benchmarks the compiled evaluation of a domain that is not present in the policy.
//
// goos: darwin
// goarch: arm64
// pkg: github.com/alibaba/opensandbox/egress/pkg/policy
// cpu: Apple M2 Pro
// BenchmarkEvaluateCompiledIndexMiss
// BenchmarkEvaluateCompiledIndexMiss/rules_500
// BenchmarkEvaluateCompiledIndexMiss/rules_500-10         	25609082	        45.57 ns/op	       0 B/op	       0 allocs/op
// BenchmarkEvaluateCompiledIndexMiss/rules_1000
// BenchmarkEvaluateCompiledIndexMiss/rules_1000-10        	26226450	        46.85 ns/op	       0 B/op	       0 allocs/op
// BenchmarkEvaluateCompiledIndexMiss/rules_10000
// BenchmarkEvaluateCompiledIndexMiss/rules_10000-10       	26390857	        45.27 ns/op	       0 B/op	       0 allocs/op
func BenchmarkEvaluateCompiledIndexMiss(b *testing.B) {
	for _, ruleCount := range []int{500, 1000, 10000} {
		b.Run(fmt.Sprintf("rules_%d", ruleCount), func(b *testing.B) {
			p := buildDomainOnlyPolicy(ruleCount, true)
			query := "not-found.example.com."

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = p.Evaluate(query)
			}
		})
	}
}

func buildDomainOnlyPolicy(ruleCount int, compiled bool) *NetworkPolicy {
	egress := make([]EgressRule, 0, ruleCount)
	for i := 0; i < ruleCount; i++ {
		egress = append(egress, EgressRule{
			Action:     ActionAllow,
			Target:     fmt.Sprintf("rule-%d.example.com", i),
			targetKind: targetDomain,
		})
	}
	p := &NetworkPolicy{
		Egress:        egress,
		DefaultAction: ActionDeny,
	}
	if compiled {
		return ensureDefaults(p)
	}
	return p
}
