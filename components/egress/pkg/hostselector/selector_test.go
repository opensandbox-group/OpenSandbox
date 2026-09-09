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

package hostselector

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestConformance(t *testing.T) {
	data, err := os.ReadFile("../../testdata/host_selectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Normalize []struct {
			Input, Want string
			Error       bool
		}
		Canonical []struct {
			Input string
			Valid bool
		}
		Matches []struct {
			Selector, Host string
			Want           bool
		}
		Overlaps []struct {
			Left, Right string
			Want        bool
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases.Normalize {
		t.Run("normalize/"+c.Input, func(t *testing.T) {
			s, err := Parse(c.Input)
			if (err != nil) != c.Error {
				t.Fatalf("error = %v, want error %v", err, c.Error)
			}
			if err == nil && s.String() != c.Want {
				t.Fatalf("got %q, want %q", s.String(), c.Want)
			}
		})
	}
	for _, c := range cases.Canonical {
		if _, err := ParseCanonical(c.Input); (err == nil) != c.Valid {
			t.Errorf("canonical %q: %v", c.Input, err)
		}
	}
	for _, c := range cases.Matches {
		s, err := ParseCanonical(c.Selector)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Matches(c.Host); got != c.Want {
			t.Errorf("%q matches %q = %v", c.Selector, c.Host, got)
		}
	}
	for _, c := range cases.Overlaps {
		a, err := ParseCanonical(c.Left)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseCanonical(c.Right)
		if err != nil {
			t.Fatal(err)
		}
		if a.Overlaps(b) != c.Want || b.Overlaps(a) != c.Want {
			t.Errorf("overlap %q / %q want %v", c.Left, c.Right, c.Want)
		}
	}
}

func TestLengthAndZeroValue(t *testing.T) {
	host := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	if len(host) != 253 {
		t.Fatal("bad fixture")
	}
	if _, err := Parse(host); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(host + "x"); err == nil {
		t.Fatal("accepted oversized hostname")
	}
	if _, err := Parse("*." + host); err == nil {
		t.Fatal("accepted wildcard with no possible member")
	}
	var zero Selector
	valid, _ := Parse("example.com")
	if zero.Matches("") || zero.Overlaps(valid) || valid.Overlaps(zero) {
		t.Fatal("zero selector must match nothing")
	}
}

func FuzzParse(f *testing.F) {
	for _, raw := range []string{"example.com", "*.BÜCHER.example.", "faß.de", "a..com", "127.0.0.1", ""} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		s, err := Parse(raw)
		if err != nil {
			return
		}
		canonical, err := ParseCanonical(s.String())
		if err != nil || canonical != s {
			t.Fatalf("canonical round trip failed for %q", raw)
		}
		again, err := Parse(s.String())
		if err != nil || again != s || !s.Overlaps(s) {
			t.Fatalf("normalization is not idempotent for %q", raw)
		}
		witness := s.base
		if s.wildcard {
			witness = "a." + witness
		}
		if !s.Matches(witness) {
			t.Fatalf("selector has no expected member: %q", raw)
		}
	})
}
