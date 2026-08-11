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

package api

import (
	"errors"
	"fmt"
	"testing"
)

type classifiedError struct {
	retryable bool
}

func (e classifiedError) Error() string   { return "classified error" }
func (e classifiedError) Retryable() bool { return e.retryable }

func TestIsRetryableError(t *testing.T) {
	if IsRetryableError(nil) {
		t.Fatal("nil error must not be retryable")
	}
	if !IsRetryableError(errors.New("unclassified")) {
		t.Fatal("unclassified errors must remain retryable")
	}
	if !IsRetryableError(classifiedError{retryable: true}) {
		t.Fatal("retryable classified error was rejected")
	}
	if IsRetryableError(fmt.Errorf("wrapped: %w", classifiedError{})) {
		t.Fatal("wrapped non-retryable error was not preserved")
	}
}

func TestPermanent(t *testing.T) {
	want := errors.New("permanent")
	err := Permanent(want)
	if !errors.Is(err, want) || IsRetryableError(err) {
		t.Fatalf("Permanent() error=%v retryable=%v", err, IsRetryableError(err))
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must remain nil")
	}
}
