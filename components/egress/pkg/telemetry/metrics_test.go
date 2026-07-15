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
	"go.opentelemetry.io/otel/attribute"

	inttelemetry "github.com/alibaba/opensandbox/internal/telemetry"
)

func TestAppendMetricAttrsFromKeyValuePairs(t *testing.T) {
	var base []attribute.KeyValue
	out := inttelemetry.AppendAttrsFromKeyValuePairs(base, "a=b")
	assert.Len(t, out, 1)
	assert.Equal(t, "a", string(out[0].Key))
	assert.Equal(t, "b", out[0].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "  foo=bar  , baz=qux ")
	assert.Len(t, out, 2)
	assert.Equal(t, "foo", string(out[0].Key))
	assert.Equal(t, "bar", out[0].Value.AsString())
	assert.Equal(t, "baz", string(out[1].Key))
	assert.Equal(t, "qux", out[1].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "k=v=x")
	assert.Len(t, out, 1)
	assert.Equal(t, "k", string(out[0].Key))
	assert.Equal(t, "v=x", out[0].Value.AsString())

	out = inttelemetry.AppendAttrsFromKeyValuePairs(nil, "novalue=,=bad,nokv")
	assert.Len(t, out, 0)
}
