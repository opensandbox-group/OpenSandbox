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

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseURIRoute_TwoSegmentsNoSignature(t *testing.T) {
	pr, err := parseURIRoute("/my-sandbox/8080")
	assert.NoError(t, err)
	assert.Equal(t, "my-sandbox", pr.sandboxID)
	assert.Equal(t, 8080, pr.port)
	assert.Equal(t, "", pr.signature)
	assert.Equal(t, "/", pr.requestURI)
	assert.False(t, pr.uriParsedAsOSEP)
}

func TestParseURIRoute_LegacyLeadingZeroPortRejected(t *testing.T) {
	_, err := parseURIRoute("/sb/08080")
	assert.Error(t, err)
}

func TestParseURIRoute_OSEPFourSegmentsWithSig(t *testing.T) {
	exp, sig := "1a2b3c", "01234567a"
	pr, err := parseURIRoute("/sb/9090/" + exp + "/" + sig + "/extra/path")
	assert.NoError(t, err)
	assert.Equal(t, "sb", pr.sandboxID)
	assert.Equal(t, 9090, pr.port)
	assert.Equal(t, exp, pr.expiresB36)
	assert.Equal(t, sig, pr.signature)
	assert.Equal(t, "/extra/path", pr.requestURI)
	assert.True(t, pr.uriParsedAsOSEP)
}
