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

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyFileSecurityNeedsFinalSync(t *testing.T) {
	require.False(t, (policyFileSecurity{}).needsFinalSync())
	require.False(t, (policyFileSecurity{selinux: "system_u:object_r:policy_t:s0\x00"}).needsFinalSync())
	require.True(t, (policyFileSecurity{ima: true}).needsFinalSync())
	require.True(t, (policyFileSecurity{evm: true}).needsFinalSync())
	require.True(t, (policyFileSecurity{selinux: "label\x00", ima: true, evm: true}).needsFinalSync())
}
