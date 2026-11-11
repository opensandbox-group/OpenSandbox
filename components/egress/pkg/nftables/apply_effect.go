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

package nftables

// ApplyEffect describes the kernel effect of a static apply attempt. Unknown
// does not imply rollback, atomicity, or that any particular rule was installed.
type ApplyEffect uint8

const (
	ApplyUnknown ApplyEffect = iota
	ApplyUnchanged
	ApplyCommitted
)

// ApplyError carries a trusted no-execution classification from a runner or
// local ruleset construction. Once a process starts, errors must be Unknown.
type ApplyError struct {
	Effect ApplyEffect
	Err    error
}

func (e *ApplyError) Error() string { return e.Err.Error() }
func (e *ApplyError) Unwrap() error { return e.Err }

// ApplyEffectOf treats nil as successful completion and unclassified errors
// conservatively. A failing operation cannot claim Committed through an error.
func ApplyEffectOf(err error) ApplyEffect {
	if err == nil {
		return ApplyCommitted
	}
	if effect, ok := err.(*ApplyError); ok {
		if effect != nil && effect.Effect == ApplyUnchanged {
			return ApplyUnchanged
		}
		return ApplyUnknown
	}
	// Joined failures are unchanged only when every attempt is classified.
	// Looking up just the first typed error could hide an unclassified sibling.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return ApplyUnknown
		}
		for _, child := range children {
			if ApplyEffectOf(child) != ApplyUnchanged {
				return ApplyUnknown
			}
		}
		return ApplyUnchanged
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if ApplyEffectOf(wrapped.Unwrap()) == ApplyUnchanged {
			return ApplyUnchanged
		}
	}
	return ApplyUnknown
}
