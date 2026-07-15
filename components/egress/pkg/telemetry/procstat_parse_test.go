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

func TestProcStatCPUJiffies(t *testing.T) {
	// synthetic: user nice system idle iowait
	line := "cpu 100 0 50 200 25 0 0 0 0 0"
	total, idle, ok := procStatCPUJiffies(line)
	assert.True(t, ok)
	assert.Equal(t, uint64(375), total)
	assert.Equal(t, uint64(225), idle) // 200+25
}
