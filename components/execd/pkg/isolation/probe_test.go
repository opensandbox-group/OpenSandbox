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

func TestSetBwrapModeAvailability(t *testing.T) {
	probeErr := errors.New("probe failed")
	tests := []struct {
		name          string
		setprivErr    error
		identityErr   error
		setprivNetErr error
		usernsErr     error
		usernsNetErr  error
		wantAvailable bool
		wantSetpriv   bool
		wantIdentity  bool
		wantSetprivN  bool
		wantUserns    bool
		wantUsernsN   bool
	}{
		{
			name:          "all available",
			wantAvailable: true, wantSetpriv: true, wantIdentity: true,
			wantSetprivN: true, wantUserns: true, wantUsernsN: true,
		},
		{
			name:      "setpriv only",
			usernsErr: probeErr, usernsNetErr: probeErr,
			wantAvailable: true, wantSetpriv: true, wantIdentity: true, wantSetprivN: true,
		},
		{
			name:        "setpriv default only",
			identityErr: probeErr, usernsErr: probeErr, usernsNetErr: probeErr,
			// The identity-switch failure does not gate the independent
			// private-net smoke for the base setpriv path.
			wantAvailable: true, wantSetpriv: true, wantSetprivN: true,
		},
		{
			name:       "userns only",
			setprivErr: probeErr, setprivNetErr: probeErr,
			wantAvailable: true, wantUserns: true, wantUsernsN: true,
		},
		{
			name:       "neither available",
			setprivErr: probeErr, setprivNetErr: probeErr,
			usernsErr: probeErr, usernsNetErr: probeErr,
		},
		{
			name:          "modes available without private net",
			setprivNetErr: probeErr, usernsNetErr: probeErr,
			wantAvailable: true, wantSetpriv: true, wantIdentity: true, wantUserns: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ProbeResult{}
			setBwrapModeAvailability(
				&result,
				tt.setprivErr,
				tt.identityErr,
				tt.setprivNetErr,
				tt.usernsErr,
				tt.usernsNetErr,
			)
			if result.Available != tt.wantAvailable {
				t.Errorf("Available = %v, want %v", result.Available, tt.wantAvailable)
			}
			if result.SetprivAvailable != tt.wantSetpriv {
				t.Errorf("SetprivAvailable = %v, want %v", result.SetprivAvailable, tt.wantSetpriv)
			}
			if result.SetprivSwitchAvailable != tt.wantIdentity {
				t.Errorf("SetprivSwitchAvailable = %v, want %v", result.SetprivSwitchAvailable, tt.wantIdentity)
			}
			if result.SetprivNetAvailable != tt.wantSetprivN {
				t.Errorf("SetprivNetAvailable = %v, want %v", result.SetprivNetAvailable, tt.wantSetprivN)
			}
			if result.UsernsAvailable != tt.wantUserns {
				t.Errorf("UsernsAvailable = %v, want %v", result.UsernsAvailable, tt.wantUserns)
			}
			if result.UsernsNetAvailable != tt.wantUsernsN {
				t.Errorf("UsernsNetAvailable = %v, want %v", result.UsernsNetAvailable, tt.wantUsernsN)
			}
		})
	}
}

func TestSmokeLabel(t *testing.T) {
	tests := []struct {
		mode       UidMode
		privateNet bool
		want       string
	}{
		{UidModeSetpriv, false, "setpriv"},
		{UidModeSetpriv, true, "setpriv+private-net"},
		{UidModeUserns, false, "userns"},
		{UidModeUserns, true, "userns+private-net"},
	}
	for _, tt := range tests {
		if got := smokeLabel(tt.mode, tt.privateNet); got != tt.want {
			t.Errorf("smokeLabel(%q, %v) = %q, want %q", tt.mode, tt.privateNet, got, tt.want)
		}
	}
}

func TestSetprivSmokeTargetIDs(t *testing.T) {
	uid, gid := setprivSmokeTargetIDs(65534, 65534)
	if uid == 0 || uid == 65534 || gid == 0 || gid == 65534 {
		t.Fatalf("targets = (%d, %d), want non-zero IDs different from current", uid, gid)
	}
}

func TestBwrapSmokeArgs(t *testing.T) {
	rootSetprivArgs := bwrapSmokeArgs(UidModeSetpriv, false, false, 0, 0)
	if slices.Contains(rootSetprivArgs, "setpriv") {
		t.Errorf("root setpriv smoke args unexpectedly contain setpriv: %v", rootSetprivArgs)
	}

	setprivArgs := bwrapSmokeArgs(UidModeSetpriv, false, false, 65534, 65533)
	if slices.Contains(setprivArgs, "--unshare-user") {
		t.Errorf("setpriv smoke args unexpectedly contain --unshare-user: %v", setprivArgs)
	}
	for _, want := range []string{"setpriv", "--reuid=65534", "--regid=65533", "--clear-groups"} {
		if !slices.Contains(setprivArgs, want) {
			t.Errorf("setpriv smoke args do not contain %q: %v", want, setprivArgs)
		}
	}

	setprivNetArgs := bwrapSmokeArgs(UidModeSetpriv, false, true, 0, 0)
	if !slices.Contains(setprivNetArgs, "--unshare-net") {
		t.Errorf("setpriv private-net smoke args do not contain --unshare-net: %v", setprivNetArgs)
	}

	usernsArgs := bwrapSmokeArgs(UidModeUserns, false, false, 1000, 1001)
	for _, want := range []string{"--unshare-user", "--disable-userns", "--uid", "1000", "--gid", "1001"} {
		if !slices.Contains(usernsArgs, want) {
			t.Errorf("userns smoke args do not contain %q: %v", want, usernsArgs)
		}
	}
	if slices.Contains(usernsArgs, "setpriv") {
		t.Errorf("userns smoke args unexpectedly contain setpriv: %v", usernsArgs)
	}

	usernsNetArgs := bwrapSmokeArgs(UidModeUserns, false, true, 1000, 1001)
	if !slices.Contains(usernsNetArgs, "--unshare-net") {
		t.Errorf("userns private-net smoke args do not contain --unshare-net: %v", usernsNetArgs)
	}

	setuidUsernsArgs := bwrapSmokeArgs(UidModeUserns, true, false, 1000, 1001)
	if slices.Contains(setuidUsernsArgs, "--disable-userns") {
		t.Errorf("setuid bwrap smoke args unexpectedly contain --disable-userns: %v", setuidUsernsArgs)
	}
}
