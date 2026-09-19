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

package main

import (
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func TestUpstreamProxySpecForProfile(t *testing.T) {
	tests := []struct {
		name        string
		proxy       string
		auth        string
		transparent string
		profile     string
		wantSpec    bool
		wantErrSubs []string
	}{
		{
			name: "no proxy env and transparent off is inert",
		},
		{
			name:     "proxy without transparent fails",
			proxy:    "http://127.0.0.1:3128",
			wantSpec: false,
			wantErrSubs: []string{
				constants.EnvUpstreamProxy,
				constants.EnvMitmproxyTransparent,
			},
		},
		{
			name: "auth without proxy fails even when transparent off",
			auth: "Basic dGVzdDp0ZXN0",
			wantErrSubs: []string{
				constants.EnvUpstreamProxyAuth,
				constants.EnvUpstreamProxy,
			},
		},
		{
			name:        "malformed proxy fails even when transparent off",
			proxy:       "://not-a-url",
			wantErrSubs: []string{constants.EnvUpstreamProxy},
		},
		{
			name:        "proxy with transparent on succeeds for default profile",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			wantSpec:    true,
		},
		{
			name:        "proxy with transparent on succeeds for sidecar profile",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			profile:     "sidecar",
			wantSpec:    true,
		},
		{
			name:        "proxy is rejected under fast-sandbox profile",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			profile:     constants.ProfileFastSandbox,
			wantErrSubs: []string{
				constants.EnvUpstreamProxy,
				constants.EnvEgressProfile,
				constants.ProfileFastSandbox,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvUpstreamProxy, tc.proxy)
			t.Setenv(constants.EnvUpstreamProxyAuth, tc.auth)
			t.Setenv(constants.EnvMitmproxyTransparent, tc.transparent)

			spec, err := upstreamProxySpecForProfile(tc.profile)
			if len(tc.wantErrSubs) > 0 {
				if err == nil {
					t.Fatalf("expected error containing %v, got nil", tc.wantErrSubs)
				}
				for _, sub := range tc.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("error %q missing %q", err.Error(), sub)
					}
				}
				if spec != nil {
					t.Fatalf("expected nil spec on error, got %+v", spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSpec {
				if spec == nil {
					t.Fatal("expected parsed spec, got nil")
				}
				if spec.Host != "proxy.local" || spec.Port != 3128 {
					t.Fatalf("unexpected spec %+v", spec)
				}
			} else if spec != nil {
				t.Fatalf("expected nil spec, got %+v", spec)
			}
		})
	}
}
