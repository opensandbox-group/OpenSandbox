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

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitOnDoubleDash(t *testing.T) {
	cases := []struct {
		name           string
		in             []string
		wantSupArgs    []string
		wantWorkerArgs []string
	}{
		{
			name:           "typical: flags then worker",
			in:             []string{"--flag=a", "--", "/bin/egress", "-foo"},
			wantSupArgs:    []string{"--flag=a"},
			wantWorkerArgs: []string{"/bin/egress", "-foo"},
		},
		{
			name:           "no double-dash returns nil worker, args untouched",
			in:             []string{"--flag=a", "/bin/egress"},
			wantSupArgs:    []string{"--flag=a", "/bin/egress"},
			wantWorkerArgs: nil,
		},
		{
			name:        "trailing double-dash, no worker",
			in:          []string{"--flag=a", "--"},
			wantSupArgs: []string{"--flag=a"},
			// append(nil, emptySlice...) returns nil, not []string{}.
			wantWorkerArgs: nil,
		},
		{
			name:           "second '--' belongs to worker argv",
			in:             []string{"--flag=a", "--", "/bin/sh", "-c", "foo -- bar"},
			wantSupArgs:    []string{"--flag=a"},
			wantWorkerArgs: []string{"/bin/sh", "-c", "foo -- bar"},
		},
		{
			name:           "double-dash first means no supervisor flags",
			in:             []string{"--", "/bin/egress"},
			wantSupArgs:    []string{},
			wantWorkerArgs: []string{"/bin/egress"},
		},
		{
			name:           "empty input",
			in:             []string{},
			wantSupArgs:    nil, // append(nil, []string{}...) returns nil
			wantWorkerArgs: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string(nil), c.in...)
			gotWorker := splitOnDoubleDash(&args)
			if !reflect.DeepEqual(args, c.wantSupArgs) {
				t.Errorf("supervisor args = %v, want %v", args, c.wantSupArgs)
			}
			if !reflect.DeepEqual(gotWorker, c.wantWorkerArgs) {
				t.Errorf("worker args = %v, want %v", gotWorker, c.wantWorkerArgs)
			}
		})
	}
}

func TestToHooks(t *testing.T) {
	if got := toHooks(nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := toHooks([]string{}); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
	got := toHooks([]string{"/a/b.sh", "/c/d.sh"})
	if len(got) != 2 || got[0].Argv[0] != "/a/b.sh" || got[1].Argv[0] != "/c/d.sh" {
		t.Errorf("unexpected hooks: %+v", got)
	}
}

func TestOpenEventLog_StderrWhenEmpty(t *testing.T) {
	w, closer, err := openEventLog("")
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	if w != os.Stderr {
		t.Errorf("empty path should return os.Stderr, got %T", w)
	}
}

func TestOpenEventLog_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "deeper", "events.jsonl")
	w, closer, err := openEventLog(target)
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	if _, statErr := os.Stat(filepath.Dir(target)); statErr != nil {
		t.Errorf("parent dir not created: %v", statErr)
	}
	if w == nil {
		t.Error("writer is nil")
	}
}

func TestEventLogDest(t *testing.T) {
	if eventLogDest("") != "stderr" {
		t.Error("empty path label")
	}
	if eventLogDest("/var/log/x.jsonl") != "/var/log/x.jsonl" {
		t.Error("path label")
	}
}
