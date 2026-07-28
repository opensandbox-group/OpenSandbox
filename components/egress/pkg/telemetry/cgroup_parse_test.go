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

import "testing"

func TestCgroupSingleValueBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		data   string
		want   int64
		wantOK bool
	}{
		{name: "memory.current", data: "8036352\n", want: 8036352, wantOK: true},
		{name: "no trailing newline", data: "4096", want: 4096, wantOK: true},
		{name: "zero is a real value", data: "0\n", want: 0, wantOK: true},
		// "max" means unlimited, not a measurement.
		{name: "max", data: "max\n", wantOK: false},
		{name: "empty", data: "", wantOK: false},
		{name: "not a number", data: "eight\n", wantOK: false},
		{name: "negative", data: "-1\n", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cgroupSingleValueBytes([]byte(tc.data))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCgroupCPUSecondsFromStat(t *testing.T) {
	t.Parallel()

	// Real cpu.stat: usage_usec is not the first field, and more follow it.
	stat := "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\nnr_periods 0\n"
	got, ok := cgroupCPUSecondsFromStat([]byte(stat))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 1.5 {
		t.Errorf("seconds = %v, want 1.5", got)
	}

	// user_usec must not be mistaken for usage_usec.
	if _, ok := cgroupCPUSecondsFromStat([]byte("user_usec 1000000\n")); ok {
		t.Error("matched a field that is not usage_usec")
	}
	if _, ok := cgroupCPUSecondsFromStat([]byte("usage_usec notanumber\n")); ok {
		t.Error("accepted an unparseable usage_usec")
	}
	if _, ok := cgroupCPUSecondsFromStat(nil); ok {
		t.Error("accepted empty cpu.stat")
	}
}

func TestCgroupCPUSecondsFromNanos(t *testing.T) {
	t.Parallel()

	got, ok := cgroupCPUSecondsFromNanos([]byte("2500000000\n"))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 2.5 {
		t.Errorf("seconds = %v, want 2.5", got)
	}
}
