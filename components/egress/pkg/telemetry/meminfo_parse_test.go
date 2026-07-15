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

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMeminfoUsedBytesFromContent(t *testing.T) {
	const sample = `MemTotal:       8000000 kB
MemFree:        1000000 kB
MemAvailable:   5000000 kB
`
	// (8000000 - 5000000) * 1024
	assert.Equal(t, int64(3000000*1024), meminfoUsedBytesFromContent([]byte(sample)))

	fallback := `MemTotal:       8000000 kB
MemFree:        1000000 kB
`
	assert.Equal(t, int64(7000000*1024), meminfoUsedBytesFromContent([]byte(fallback)))
}
