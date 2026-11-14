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

package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/require"
)

func TestFastSandboxSubjectTargetD2Golden(t *testing.T) {
	for _, tt := range []struct{ runtime, attachment, digest string }{
		{"", "", "edf2578f5da24904066ebc0a416f27ce57072fb27efe10fee161ba4362554d1b"},
		{"runtime", "attachment", "b46aad8c07e9bb4e59e780efbbd321468723bf206fda97f75314ad3d47c6f23b"},
		{"a\"\\\n\t<>&\u2028", "附加\u2029/", "a767375d57654c4f96eadd98196d8dba3d4338be083e6fc4468419e3de9c664b"},
		{"a", "bc", "9a09388f9987506b5f89f7f707f87780004ef8ec0a2825e8523ea159c44b5987"},
		{"ab", "c", "719f002a56a960a864930e95fec45298efd076e99757df725497ff65dc09e9d8"},
		{strings.Repeat("r", 128), strings.Repeat("a", 256), "41c73d6357c5e771cc9c2d933586c1cd23e329c36db56c90d81f9ef913bd5903"},
	} {
		target, err := fastSandboxSubjectTarget("sandbox", subject.Fencing{RuntimeInstanceID: tt.runtime, AttachmentID: tt.attachment})
		require.NoError(t, err)
		require.Equal(t, string(subject.FromSandboxUID("sandbox")), target.SubjectKey)
		require.Equal(t, tt.digest, target.SubjectGeneration)
		require.Regexp(t, "^[0-9a-f]{64}$", target.SubjectGeneration)
	}
}

func TestFastSandboxSubjectTargetLimits(t *testing.T) {
	for _, uid := range []string{"x", strings.Repeat("x", 254), strings.Repeat("界", 254)} {
		target, err := fastSandboxSubjectTarget(uid, subject.Fencing{})
		require.NoError(t, err)
		require.Equal(t, string(subject.FromSandboxUID(uid)), target.SubjectKey)
		require.LessOrEqual(t, utf8.RuneCountInString(target.SubjectKey), 256)
	}
	for _, uid := range []string{"", strings.Repeat("界", 255), string([]byte{0xff})} {
		_, err := fastSandboxSubjectTarget(uid, subject.Fencing{})
		require.ErrorIs(t, err, revision.ErrInvalid)
	}
	// encoding/json replaces invalid UTF-8; reject it before hashing instead of
	// letting distinct invalid fences collapse onto the replacement character.
	for _, fence := range []subject.Fencing{{RuntimeInstanceID: string([]byte{0xff})}, {AttachmentID: string([]byte{0xff})}} {
		_, err := fastSandboxSubjectTarget("uid", fence)
		require.ErrorIs(t, err, revision.ErrInvalid)
	}
}
