// Copyright 2025 The OpenSandbox Authors
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
	"reflect"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func TestParseCacheNamespaces(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]cache.Config
	}{
		{name: "empty stays cluster-wide", input: "", want: nil},
		{name: "blank stays cluster-wide", input: "   ", want: nil},
		{name: "separators only stay cluster-wide", input: " , , ", want: nil},
		{name: "single namespace", input: "opensandbox", want: map[string]cache.Config{"opensandbox": {}}},
		{name: "trims whitespace and drops empty entries", input: " a , ,b,", want: map[string]cache.Config{"a": {}, "b": {}}},
		{name: "deduplicates", input: "a,a", want: map[string]cache.Config{"a": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCacheNamespaces(tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCacheNamespaces(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
