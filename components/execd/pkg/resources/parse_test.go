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

package resources

import (
	"testing"
	"time"
)

func TestParseMemoryStatField(t *testing.T) {
	data := []byte("anon 1000\nfile 2000\ninactive_file 1500\ntotal_inactive_file 3000\n")
	if got, ok := parseMemoryStatField(data, "inactive_file"); !ok || got != 1500 {
		t.Fatalf("inactive_file = (%d, %v), want (1500, true)", got, ok)
	}
	if got, ok := parseMemoryStatField(data, "total_inactive_file"); !ok || got != 3000 {
		t.Fatalf("total_inactive_file = (%d, %v), want (3000, true)", got, ok)
	}
	if _, ok := parseMemoryStatField(data, "missing"); ok {
		t.Fatal("missing field should fail")
	}
	if _, ok := parseMemoryStatField([]byte("inactive_file abc\n"), "inactive_file"); ok {
		t.Fatal("malformed value should fail")
	}
}

func TestParseCPUStatUsageUSec(t *testing.T) {
	data := []byte("usage_usec 123456\nuser_usec 1000\nsystem_usec 2000\nnr_periods 0\n")
	if got, ok := parseCPUStatUsageUSec(data); !ok || got != 123456 {
		t.Fatalf("parseCPUStatUsageUSec = (%d, %v), want (123456, true)", got, ok)
	}
	for _, data := range []string{"", "user_usec 1\n", "usage_usec abc\n", "usage_usec\n"} {
		if _, ok := parseCPUStatUsageUSec([]byte(data)); ok {
			t.Fatalf("parseCPUStatUsageUSec(%q) should fail", data)
		}
	}
}

func TestParseSingleUint(t *testing.T) {
	if got, ok := parseSingleUint([]byte("  50000000\n")); !ok || got != 50000000 {
		t.Fatalf("parseSingleUint = (%d, %v), want (50000000, true)", got, ok)
	}
	if _, ok := parseSingleUint([]byte("-1")); ok {
		t.Fatal("parseSingleUint(-1) should fail")
	}
}

func TestParseCPUMax(t *testing.T) {
	quota, period, limited, ok := parseCPUMax([]byte("100000 100000\n"))
	if !ok || !limited || quota != 100000 || period != 100000 {
		t.Fatalf("parseCPUMax = (%d, %d, %v, %v), want limited 1-core quota", quota, period, limited, ok)
	}
	if _, period, limited, ok = parseCPUMax([]byte("max 100000\n")); !ok || limited || period != 100000 {
		t.Fatalf("parseCPUMax(max) = limited=%v ok=%v, want unlimited ok", limited, ok)
	}
	for _, data := range []string{"", "100000\n", "max\n", "abc 100000\n", "100000 abc\n", "100000 0\n", "-5 100000\n"} {
		if _, _, _, ok := parseCPUMax([]byte(data)); ok {
			t.Fatalf("parseCPUMax(%q) should fail", data)
		}
	}
}

func TestQuotaCores(t *testing.T) {
	tests := []struct {
		quota, period int64
		want          float64
	}{
		{100000, 100000, 1},
		{200000, 100000, 2},
		{150000, 100000, 2}, // rounds up to whole cores
		{50000, 100000, 1},
		{0, 100000, 0},
		{100000, 0, 0},
		{-1, 100000, 0},
	}
	for _, tt := range tests {
		if got := quotaCores(tt.quota, tt.period); got != tt.want {
			t.Fatalf("quotaCores(%d, %d) = %v, want %v", tt.quota, tt.period, got, tt.want)
		}
	}
}

func TestParseMemoryMax(t *testing.T) {
	if limit, limited, ok := parseMemoryMax([]byte("209715200\n")); !ok || !limited || limit != 209715200 {
		t.Fatalf("parseMemoryMax = (%d, %v, %v), want (209715200, true, true)", limit, limited, ok)
	}
	if _, limited, ok := parseMemoryMax([]byte("max\n")); !ok || limited {
		t.Fatalf("parseMemoryMax(max) = limited=%v ok=%v, want unlimited ok", limited, ok)
	}
	if _, _, ok := parseMemoryMax([]byte("abc\n")); ok {
		t.Fatal("parseMemoryMax(abc) should fail")
	}
}

func TestParseV1MemoryLimit(t *testing.T) {
	limit, limited, ok := parseV1MemoryLimit([]byte("2147483648\n"))
	if !ok || !limited || limit != 2147483648 {
		t.Fatalf("parseV1MemoryLimit = (%d, %v, %v), want limited", limit, limited, ok)
	}
	if _, limited, ok := parseV1MemoryLimit([]byte("9223372036854771712\n")); !ok || limited {
		t.Fatalf("parseV1MemoryLimit(huge) = limited=%v ok=%v, want unlimited ok", limited, ok)
	}
}

func TestParseV1QuotaUS(t *testing.T) {
	if quota, limited, ok := parseV1QuotaUS([]byte("200000\n")); !ok || !limited || quota != 200000 {
		t.Fatalf("parseV1QuotaUS = (%d, %v, %v), want limited 200000", quota, limited, ok)
	}
	if _, limited, ok := parseV1QuotaUS([]byte("-1\n")); !ok || limited {
		t.Fatalf("parseV1QuotaUS(-1) = limited=%v ok=%v, want unlimited ok", limited, ok)
	}
	if _, _, ok := parseV1QuotaUS([]byte("abc\n")); ok {
		t.Fatal("parseV1QuotaUS(abc) should fail")
	}
}

func TestCPUDeltaPercent(t *testing.T) {
	// 1 core-second of usage in a 1s window on a 2-core sandbox = 50%.
	if got := cpuDeltaPercent(0, 1e6, time.Second, 2); got != 50 {
		t.Fatalf("cpuDeltaPercent = %v, want 50", got)
	}
	if got := cpuDeltaPercent(0, 0, time.Second, 2); got != 0 {
		t.Fatalf("cpuDeltaPercent(no usage) = %v, want 0", got)
	}
	if got := cpuDeltaPercent(100, 50, time.Second, 2); got != 0 {
		t.Fatalf("cpuDeltaPercent(regressed counter) = %v, want 0", got)
	}
	if got := cpuDeltaPercent(0, 100, 0, 2); got != 0 {
		t.Fatalf("cpuDeltaPercent(zero wall) = %v, want 0", got)
	}
	if got := cpuDeltaPercent(0, 100, time.Second, 0); got != 0 {
		t.Fatalf("cpuDeltaPercent(zero capacity) = %v, want 0", got)
	}
}
