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

package web

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParseFilesystemID(t *testing.T) {
	for _, raw := range []string{"", "-1", "+1", "1.0", " 1", "1 ", "4294967295", "4294967296", "abc"} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseFilesystemID(raw)
			require.Error(t, err)
		})
	}
	for raw, want := range map[string]uint32{"0": 0, "1001": 1001, "4294967294": 4294967294} {
		actual, err := parseFilesystemID(raw)
		require.NoError(t, err)
		require.Equal(t, want, actual)
	}
}
