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

package renewintent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewIntent(t *testing.T) {
	intent := NewIntent("sb-123", 8080, "/api/foo")
	assert.Equal(t, "sb-123", intent.SandboxID)
	assert.Equal(t, 8080, intent.Port)
	assert.Equal(t, "/api/foo", intent.RequestURI)
	assert.NotEmpty(t, intent.ObservedAt)
}

func TestIntent_JSONRoundTrip(t *testing.T) {
	intent := NewIntent("my-sandbox", 80, "/")
	data, err := json.Marshal(intent)
	assert.NoError(t, err)
	var decoded Intent
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, intent.SandboxID, decoded.SandboxID)
	assert.Equal(t, intent.Port, decoded.Port)
	assert.Equal(t, intent.RequestURI, decoded.RequestURI)
}

func TestIntent_JSONHasRequiredFields(t *testing.T) {
	intent := NewIntent("id", 0, "")
	data, err := json.Marshal(intent)
	assert.NoError(t, err)
	var m map[string]interface{}
	err = json.Unmarshal(data, &m)
	assert.NoError(t, err)
	for _, key := range []string{"sandbox_id", "observed_at"} {
		_, ok := m[key]
		assert.True(t, ok, "missing required JSON field %q", key)
	}
}
