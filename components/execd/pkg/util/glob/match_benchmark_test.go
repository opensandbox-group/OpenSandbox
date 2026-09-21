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

package glob

import (
	"path/filepath"
	"testing"
)

func BenchmarkPathMatch(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, tt := range matchTests {
			if tt.isStandard && tt.testOnDisk {
				pattern := filepath.FromSlash(tt.pattern)
				testPath := filepath.FromSlash(tt.testPath)
				PathMatch(pattern, testPath)
			}
		}
	}
}
