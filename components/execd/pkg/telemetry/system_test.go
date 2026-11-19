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

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/alibaba/opensandbox/execd/pkg/resources"
)

func TestDiskIORebaser(t *testing.T) {
	t.Parallel()

	r := &diskIORebaser{}

	// The first sample becomes the baseline: nothing is attributed yet.
	first := r.rebase(7, resources.DiskIO{ReadBytes: 100, WrittenBytes: 200, ReadOps: 1, WriteOps: 2})
	assert.Equal(t, resources.DiskIO{}, first)

	// Same binding: increments since the baseline.
	second := r.rebase(7, resources.DiskIO{ReadBytes: 250, WrittenBytes: 300, ReadOps: 3, WriteOps: 4})
	assert.Equal(t, resources.DiskIO{ReadBytes: 150, WrittenBytes: 100, ReadOps: 2, WriteOps: 2}, second)

	// Binding handoff: totals accumulated under the old binding are dropped,
	// so the new identity's series starts at zero instead of inheriting them.
	rebound := r.rebase(8, resources.DiskIO{ReadBytes: 900, WrittenBytes: 900, ReadOps: 9, WriteOps: 9})
	assert.Equal(t, resources.DiskIO{}, rebound)

	// Counters grow from the new baseline afterwards.
	after := r.rebase(8, resources.DiskIO{ReadBytes: 950, WrittenBytes: 900, ReadOps: 9, WriteOps: 10})
	assert.Equal(t, resources.DiskIO{ReadBytes: 50, WrittenBytes: 0, ReadOps: 0, WriteOps: 1}, after)

	// A counter that reset below the baseline clamps to zero, never negative.
	reset := r.rebase(8, resources.DiskIO{ReadBytes: 5})
	assert.Equal(t, resources.DiskIO{}, reset)
}
