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

package opensandbox

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type assertCompat struct{}
type requireCompat struct{}

var (
	assert  = assertCompat{}
	require = requireCompat{}
)

func (assertCompat) Fail(t *testing.T, failureMessage string, msgAndArgs ...any) bool {
	t.Helper()
	t.Errorf("%s", joinMessage(failureMessage, msgAndArgs...))
	return false
}

func (assertCompat) ErrorAs(t *testing.T, err error, target any, msgAndArgs ...any) bool {
	t.Helper()
	if errors.As(err, target) {
		return true
	}
	t.Errorf("%s", joinMessage(fmt.Sprintf("expected error %v to match target %T", err, target), msgAndArgs...))
	return false
}

func (assertCompat) Contains(t *testing.T, s string, contains string, msgAndArgs ...any) bool {
	t.Helper()
	if strings.Contains(s, contains) {
		return true
	}
	t.Errorf("%s", joinMessage(fmt.Sprintf("expected %q to contain %q", s, contains), msgAndArgs...))
	return false
}

func (requireCompat) FailNow(t *testing.T, failureMessage string, msgAndArgs ...any) {
	t.Helper()
	t.Fatalf("%s", joinMessage(failureMessage, msgAndArgs...))
}

func (requireCompat) NoError(t *testing.T, err error, msgAndArgs ...any) {
	t.Helper()
	if err == nil {
		return
	}
	t.Fatalf("%s", joinMessage(fmt.Sprintf("expected no error, got %v", err), msgAndArgs...))
}

func (requireCompat) NoErrorf(t *testing.T, err error, msg string, args ...any) {
	t.Helper()
	if err == nil {
		return
	}
	t.Fatalf("%s: %v", fmt.Sprintf(msg, args...), err)
}

func (requireCompat) Error(t *testing.T, err error, msgAndArgs ...any) {
	t.Helper()
	if err != nil {
		return
	}
	t.Fatalf("%s", joinMessage("expected error, got nil", msgAndArgs...))
}

func (requireCompat) ErrorAs(t *testing.T, err error, target any, msgAndArgs ...any) {
	t.Helper()
	if errors.As(err, target) {
		return
	}
	t.Fatalf("%s", joinMessage(fmt.Sprintf("expected error %v to match target %T", err, target), msgAndArgs...))
}

func (requireCompat) ErrorIs(t *testing.T, err error, target error, msgAndArgs ...any) {
	t.Helper()
	if errors.Is(err, target) {
		return
	}
	t.Fatalf("%s", joinMessage(fmt.Sprintf("expected error %v to match %v", err, target), msgAndArgs...))
}

func (requireCompat) True(t *testing.T, value bool, msgAndArgs ...any) {
	t.Helper()
	if value {
		return
	}
	t.Fatalf("%s", joinMessage("expected true, got false", msgAndArgs...))
}

func (requireCompat) Len(t *testing.T, object any, length int, msgAndArgs ...any) {
	t.Helper()
	v := reflect.ValueOf(object)
	switch v.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		if v.Len() == length {
			return
		}
		t.Fatalf("%s", joinMessage(fmt.Sprintf("expected length %d, got %d", length, v.Len()), msgAndArgs...))
	default:
		t.Fatalf("%s", joinMessage(fmt.Sprintf("cannot get length of %T", object), msgAndArgs...))
	}
}

func (requireCompat) NotNil(t *testing.T, object any, msgAndArgs ...any) {
	t.Helper()
	if !isNil(object) {
		return
	}
	t.Fatalf("%s", joinMessage("expected value not to be nil", msgAndArgs...))
}

func (requireCompat) Equal(t *testing.T, expected any, actual any, msgAndArgs ...any) {
	t.Helper()
	if reflect.DeepEqual(expected, actual) {
		return
	}
	t.Fatalf("%s", joinMessage(fmt.Sprintf("expected %v, got %v", expected, actual), msgAndArgs...))
}

func (requireCompat) NotEmpty(t *testing.T, object any, msgAndArgs ...any) {
	t.Helper()
	if !isEmpty(object) {
		return
	}
	t.Fatalf("%s", joinMessage("expected value not to be empty", msgAndArgs...))
}

func (requireCompat) LessOrEqual(t *testing.T, a any, b any, msgAndArgs ...any) {
	t.Helper()
	switch left := a.(type) {
	case time.Duration:
		right, ok := b.(time.Duration)
		if !ok {
			t.Fatalf("%s", joinMessage(fmt.Sprintf("cannot compare %T and %T", a, b), msgAndArgs...))
		}
		if left <= right {
			return
		}
		t.Fatalf("%s", joinMessage(fmt.Sprintf("expected %v <= %v", left, right), msgAndArgs...))
	default:
		t.Fatalf("%s", joinMessage(fmt.Sprintf("unsupported LessOrEqual type %T", a), msgAndArgs...))
	}
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() == 0
	default:
		zero := reflect.Zero(rv.Type()).Interface()
		return reflect.DeepEqual(v, zero)
	}
}

func joinMessage(base string, msgAndArgs ...any) string {
	if len(msgAndArgs) == 0 {
		return base
	}
	if format, ok := msgAndArgs[0].(string); ok {
		if len(msgAndArgs) == 1 {
			return fmt.Sprintf("%s: %s", base, format)
		}
		return fmt.Sprintf("%s: %s", base, fmt.Sprintf(format, msgAndArgs[1:]...))
	}
	return fmt.Sprintf("%s: %v", base, msgAndArgs)
}
