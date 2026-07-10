//go:build !linux

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
	"fmt"
	"os/exec"
)

// findBwrap returns empty string on non-Linux.
func findBwrap() string { return "" }

// bwrapStub is the non-Linux bwrap implementation. It reports Available=false
// and fails all Wrap calls.
type bwrapStub struct{}

// NewBwrap returns a stub on non-Linux platforms.
func NewBwrap(_ Config) Isolator {
	return &bwrapStub{}
}

func (b *bwrapStub) Name() string               { return "bwrap" }
func (b *bwrapStub) Available() bool            { return false }
func (b *bwrapStub) Capabilities() Capabilities { return Capabilities{Available: false} }
func (b *bwrapStub) Wrap(_ *exec.Cmd, _ WrapOptions) error {
	return fmt.Errorf("bwrap: unavailable on non-Linux platform")
}

var _ Isolator = (*bwrapStub)(nil)
