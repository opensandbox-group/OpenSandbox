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

package proxy

import (
	"fmt"
	"testing"
	"time"
)

// 每次迭代请求所有上游一轮；用 -benchtime=10x 比较十轮请求的连接复用。
func BenchmarkHTTPConnectionPool(b *testing.B) {
	for _, upstreams := range []int{50, 1000} {
		for _, capacity := range []int{100, 1000} {
			b.Run(fmt.Sprintf("upstreams=%d/capacity=%d", upstreams, capacity), func(b *testing.B) {
				routes, connections := newPoolUpstreams(b, upstreams)
				gateway := newPoolProxy(b, WithHTTPConnectionPool(capacity, 2, 90*time.Second))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					for _, route := range routes {
						requestPoolUpstream(b, gateway, route)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(connections.Load())/float64(b.N*upstreams), "new-conns/req")
			})
		}
	}
}
