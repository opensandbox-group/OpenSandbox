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
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

// Installation-only proof through the existing real Go -> Unix -> Python
// fixture. There is no disk/nft transaction, live policy/epoch publication,
// request-path activation, TLS, or public handler acknowledgement here.
func TestEffectivePolicyCandidateIPCInstallation(t *testing.T) {
	inputs := policyCandidateInputs(t)
	s := revisionIntegrationServer(t)
	s.credentialVault = policyCandidateStore(t, inputs)
	m, child, _ := revisionIntegrationLaunch(t, s)
	before, err := s.credentialVault.ActiveSnapshot()
	require.NoError(t, err)
	state, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	base, err := newEffectivePolicyBase(inputs, 0)
	require.NoError(t, err)
	next := policyCandidateInputs(t)
	next.telemetryAllow = []policy.EgressRule{{Action: "allow", Target: "metrics.example.com"}}
	candidate, err := base.Prepare(s.credentialVault, next)
	require.NoError(t, err)
	require.NoError(t, base.Validate(s.credentialVault, candidate))
	installed, err := m.revisionSession.Update(mutationContext(t), candidate.Snapshot(), candidate.PolicyEpoch())
	require.NoError(t, err)
	report := child.exchange(t, map[string]string{"command": "inspect"})
	require.NotNil(t, report.Active)
	require.Equal(t, installed, *report.Active)
	require.Equal(t, report.Active, report.View)
	require.Equal(t, candidate.PolicyEpoch(), report.Active.PolicyEpoch)
	require.Equal(t, candidate.Digest(), report.Digest)
	require.Equal(t, candidate.Digest(), report.Active.Digest)
	require.Equal(t, before.Revision, report.Active.VaultRevision)
	require.True(t, report.AdmissionDisabled)
	require.Equal(t, int64(0), base.epoch, "installation must not publish a control-plane policy epoch")
	after, err := s.credentialVault.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterState, err := s.credentialVault.Sanitized()
	require.NoError(t, err)
	require.Equal(t, state, afterState)
	require.NoError(t, base.Validate(s.credentialVault, candidate))
}
