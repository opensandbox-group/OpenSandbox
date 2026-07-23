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

package isolation

import (
	"errors"
	"slices"
	"testing"
)

func TestParseBwrapVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"bubblewrap 0.8.0\n", "0.8.0"},
		{"bwrap 0.10.0\n", "0.10.0"},
		{"bwrap: unrecognized option '--version'\n", ""},
		{"", ""},
		{"some unrelated output\nbubblewrap 0.11.2-dev\nmore output", "0.11.2"},
	}

	for _, tt := range tests {
		got := parseBwrapVersion(tt.in)
		if got != tt.want {
			t.Errorf("parseBwrapVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProbeConfigDefaults(t *testing.T) {
	cfg := ProbeConfig{
		UpperRoot:     "/var/lib/execd/isolation",
		UpperMaxBytes: 8 * 1024 * 1024 * 1024,
	}
	if cfg.UpperRoot == "" {
		t.Error("UpperRoot should not be empty")
	}
}

func TestProbeResult_Defaults(t *testing.T) {
	result := ProbeResult{}
	if result.Available {
		t.Error("default ProbeResult should have Available=false")
	}
	if result.CommitSupported {
		t.Error("default ProbeResult should have CommitSupported=false")
	}
	if result.DiffSupported {
		t.Error("default ProbeResult should have DiffSupported=false")
	}
	if result.SetprivAvailable || result.SetprivSwitchAvailable || result.UsernsAvailable {
		t.Error("default ProbeResult should have both uid modes unavailable")
	}
}

func TestSetBwrapModeAvailability(t *testing.T) {
	probeErr := errors.New("probe failed")
	tests := []struct {
		name          string
		setprivErr    error
		identityErr   error
		usernsErr     error
		wantAvailable bool
		wantSetpriv   bool
		wantIdentity  bool
		wantUserns    bool
	}{
		{name: "both available", wantAvailable: true, wantSetpriv: true, wantIdentity: true, wantUserns: true},
		{name: "setpriv only", usernsErr: probeErr, wantAvailable: true, wantSetpriv: true, wantIdentity: true},
		{name: "setpriv default only", identityErr: probeErr, usernsErr: probeErr, wantAvailable: true, wantSetpriv: true},
		{name: "userns only", setprivErr: probeErr, wantAvailable: true, wantUserns: true},
		{name: "neither available", setprivErr: probeErr, usernsErr: probeErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ProbeResult{}
			setBwrapModeAvailability(&result, tt.setprivErr, tt.identityErr, tt.usernsErr)
			if result.Available != tt.wantAvailable {
				t.Errorf("Available = %v, want %v", result.Available, tt.wantAvailable)
			}
			if result.SetprivAvailable != tt.wantSetpriv {
				t.Errorf("SetprivAvailable = %v, want %v", result.SetprivAvailable, tt.wantSetpriv)
			}
			if result.SetprivSwitchAvailable != tt.wantIdentity {
				t.Errorf("SetprivSwitchAvailable = %v, want %v", result.SetprivSwitchAvailable, tt.wantIdentity)
			}
			if result.UsernsAvailable != tt.wantUserns {
				t.Errorf("UsernsAvailable = %v, want %v", result.UsernsAvailable, tt.wantUserns)
			}
		})
	}
}

func TestSetprivSmokeTargetIDs(t *testing.T) {
	uid, gid := setprivSmokeTargetIDs(65534, 65534)
	if uid == 0 || uid == 65534 || gid == 0 || gid == 65534 {
		t.Fatalf("targets = (%d, %d), want non-zero IDs different from current", uid, gid)
	}
}

func TestBwrapSmokeArgs(t *testing.T) {
	rootSetprivArgs := bwrapSmokeArgs(UidModeSetpriv, false, 0, 0)
	if slices.Contains(rootSetprivArgs, "setpriv") {
		t.Errorf("root setpriv smoke args unexpectedly contain setpriv: %v", rootSetprivArgs)
	}

	setprivArgs := bwrapSmokeArgs(UidModeSetpriv, false, 65534, 65533)
	if slices.Contains(setprivArgs, "--unshare-user") {
		t.Errorf("setpriv smoke args unexpectedly contain --unshare-user: %v", setprivArgs)
	}
	for _, want := range []string{"setpriv", "--reuid=65534", "--regid=65533", "--clear-groups"} {
		if !slices.Contains(setprivArgs, want) {
			t.Errorf("setpriv smoke args do not contain %q: %v", want, setprivArgs)
		}
	}

	usernsArgs := bwrapSmokeArgs(UidModeUserns, false, 1000, 1001)
	for _, want := range []string{"--unshare-user", "--disable-userns", "--uid", "1000", "--gid", "1001"} {
		if !slices.Contains(usernsArgs, want) {
			t.Errorf("userns smoke args do not contain %q: %v", want, usernsArgs)
		}
	}
	if slices.Contains(usernsArgs, "setpriv") {
		t.Errorf("userns smoke args unexpectedly contain setpriv: %v", usernsArgs)
	}

	setuidUsernsArgs := bwrapSmokeArgs(UidModeUserns, true, 1000, 1001)
	if slices.Contains(setuidUsernsArgs, "--disable-userns") {
		t.Errorf("setuid bwrap smoke args unexpectedly contain --disable-userns: %v", setuidUsernsArgs)
	}
}
