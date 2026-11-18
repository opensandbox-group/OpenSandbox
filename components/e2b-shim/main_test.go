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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunHelpAndUsage(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"adapter", "--help"}, {"adapter", "-h"}, {"router", "-h"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != exitOK {
			t.Errorf("%v: exit code = %d, want 0", args, code)
		}
		if strings.Count(out.String(), "Usage: e2b-shim") != 1 || errOut.Len() != 0 {
			t.Errorf("%v: stdout = %q, stderr = %q, want usage once on stdout", args, out.String(), errOut.String())
		}
	}
}

func TestRunVersion(t *testing.T) {
	if code := run(context.Background(), []string{"version"}, &bytes.Buffer{}, &bytes.Buffer{}); code != exitOK {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"bogus"}, &out, &errOut); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunNoArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "Usage: e2b-shim") {
		t.Errorf("stderr = %q, want usage", errOut.String())
	}
}

func TestRunFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"adapter", "--bogus"},
		{"adapter", "extra-arg"},
		{"router", "--listen"},
		{"router", "--config", "e2b-shim.json"},
	} {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, &out, &errOut)
		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, exitUsage)
		}
	}
}
