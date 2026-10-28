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

package isolation

import (
	"os"
	"path/filepath"
	"testing"
)

func newConfinedView(t *testing.T) (*ConfinedView, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the production view builder: it passes the session identity
	// (or execd's own), which chown always permits.
	return NewConfinedView(root, uint32(os.Getuid()), uint32(os.Getgid())), root
}

func TestConfinedView_BasicRoundTrip(t *testing.T) {
	view, root := newConfinedView(t)

	if err := view.WriteFile("/tmp/note.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("host file = %q, %v", got, err)
	}
	data, err := view.ReadFile("/tmp/note.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	if _, err := view.Stat("/tmp/note.txt"); err != nil {
		t.Fatalf("Stat = %v", err)
	}

	if err := view.MkdirAll("/tmp/sub/deep", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := view.WriteFile("/tmp/sub/deep/a.log", []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := view.ReadDir("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["note.txt"] || !names["sub"] {
		t.Fatalf("ReadDir missing entries: %v", names)
	}

	results, err := view.Search("/tmp", "**.log")
	if err != nil || len(results) != 1 || results[0] != "sub/deep/a.log" {
		t.Fatalf("Search = %v, %v; want [sub/deep/a.log]", results, err)
	}

	if err := view.ReplaceContent("/tmp/note.txt", "hello", "world"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "note.txt")); string(got) != "world" {
		t.Fatalf("ReplaceContent left %q", got)
	}

	if err := view.Rename("/tmp/note.txt", "/tmp/sub/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "renamed.txt")); err != nil {
		t.Fatalf("rename target missing: %v", err)
	}

	if err := view.Remove("/tmp/sub/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "renamed.txt")); !os.IsNotExist(err) {
		t.Fatalf("removed file still present: %v", err)
	}
}

func TestConfinedView_RejectsSymlinkEscape(t *testing.T) {
	view, root := newConfinedView(t)
	outside := filepath.Join(filepath.Dir(root), "confined-outside.txt")
	if err := os.WriteFile(outside, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	if err := os.Symlink(outside, filepath.Join(root, "abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../"+filepath.Base(filepath.Dir(root))+"/"+filepath.Base(outside), filepath.Join(root, "dir", "rel")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/tmp/abs", "/tmp/dir/rel"} {
		if _, err := view.ReadFile(p); err == nil {
			t.Errorf("ReadFile %s through escaping symlink = nil error, want rejection", p)
		}
		if _, err := view.Stat(p); err == nil {
			t.Errorf("Stat %s through escaping symlink = nil error, want rejection", p)
		}
		if err := view.WriteFile(p, []byte("evil"), 0o644); err == nil {
			t.Errorf("WriteFile %s through escaping symlink = nil error, want rejection", p)
		}
	}

	// The outside file must be untouched.
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "host" {
		t.Fatalf("outside file = %q, %v; escape must not write through", got, err)
	}
}

func TestConfinedView_FollowsInRootSymlink(t *testing.T) {
	view, root := newConfinedView(t)
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("in-root"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Relative in-root targets match what workloads create; os.Root rejects
	// absolute link targets outright (see the ConfinedView doc comment).
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	data, err := view.ReadFile("/tmp/link.txt")
	if err != nil || string(data) != "in-root" {
		t.Fatalf("in-root symlink ReadFile = %q, %v", data, err)
	}
}

func TestConfinedView_RejectsRootRemoval(t *testing.T) {
	view, _ := newConfinedView(t)
	if err := view.Remove("/tmp"); err == nil {
		t.Error("Remove of the view root must be rejected")
	}
	if err := view.RemoveAll("/tmp"); err == nil {
		t.Error("RemoveAll of the view root must be rejected")
	}
	if _, err := view.Stat("/tmp"); err != nil {
		t.Fatalf("root must survive removal attempts: %v", err)
	}
}
