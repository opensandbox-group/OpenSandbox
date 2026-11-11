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

package mitmpassthrough

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/hostselector"
)

func TestSharedContract(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/static_passthrough.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		External []struct {
			Name      string
			Present   bool
			Input     string
			Valid     bool
			Canonical []string
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.External {
		t.Run(c.Name, func(t *testing.T) {
			cfg, err := Parse(c.Input, c.Present)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
			if err != nil {
				return
			}
			want, _ := json.Marshal(c.Canonical)
			if cfg.JSON() != string(want) {
				t.Fatalf("got %s want %s", cfg.JSON(), want)
			}
		})
	}
}
func TestLimitsAndOwnership(t *testing.T) {
	for _, raw := range []string{string([]byte{'[', '"', 255, '"', ']'}), strings.Repeat(" ", MaxBytes+1), "[" + strings.Repeat(`"a.example.com",`, MaxSelectors) + `"a.example.com"]`} {
		if _, err := Parse(raw, true); err == nil {
			t.Fatal("accepted invalid or oversized input")
		}
	}
	cfg, err := Parse(`["a.example.com"]`, true)
	if err != nil {
		t.Fatal(err)
	}
	selectors := cfg.Selectors()
	selectors[0], _ = hostselector.Parse("changed.example.com")
	selectors = append(selectors, selectors[0])
	if len(cfg.Selectors()) != 1 || cfg.Selectors()[0].String() != "a.example.com" {
		t.Fatal("mutable config")
	}
	if (Config{}).JSON() != "[]" {
		t.Fatal("zero config not empty")
	}
}

func TestSharedOverlap(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/static_passthrough.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Overlaps []struct {
			Left, Right string
			Overlap     bool
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Overlaps {
		left, err := Parse(`["`+item.Left+`"]`, true)
		if err != nil {
			t.Fatal(err)
		}
		right, err := hostselector.Parse(item.Right)
		if err != nil {
			t.Fatal(err)
		}
		if left.Overlaps(right) != item.Overlap {
			t.Fatalf("overlap mismatch: %+v", item)
		}
	}
}
func TestBoundaryLimits(t *testing.T) {
	raw := `[]` + strings.Repeat(" ", MaxBytes-2)
	if _, err := Parse(raw, true); err != nil {
		t.Fatal(err)
	}
	raw = "[" + strings.Repeat(`"a.example.com",`, MaxSelectors-1) + `"a.example.com"]`
	if _, err := Parse(raw, true); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalOutputExpansionLimit(t *testing.T) {
	entries := make([]string, MaxSelectors)
	for i := range entries {
		prefix := fmt.Sprintf("%03x.", i) + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "."
		for n := 40; n < 60; n++ {
			host := prefix + "ü" + strings.Repeat("d", n)
			selector, err := hostselector.Parse(host)
			if err == nil && len(selector.String()) == 253 {
				entries[i] = host
				break
			}
		}
		if entries[i] == "" {
			t.Fatal("could not construct output-boundary host")
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxBytes {
		t.Fatal("input must fit")
	}
	if _, err := Parse(string(raw), true); err == nil {
		t.Fatal("accepted oversized canonical output")
	}
}
