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

package state

import (
	"bytes"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestSourceStateNamespacesAreIsolatedFromEachOtherAndLegacyState(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	wantWriterID := db.WriterID()

	for source, value := range map[string]string{
		"source-a": "value-a",
		"source-b": "value-b",
		"meta":     "private-schema",
	} {
		sourceState, err := db.SourceState(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := sourceState.Update(func(tx SourceStateWriter) error {
			return tx.Put(keySchema, []byte(value))
		}); err != nil {
			t.Fatalf("SourceState(%q).Update(): %v", source, err)
		}
	}

	for source, want := range map[string]string{
		"source-a": "value-a",
		"source-b": "value-b",
		"meta":     "private-schema",
	} {
		sourceState, err := db.SourceState(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := sourceState.View(func(tx SourceStateReader) error {
			got, found, err := tx.Get(keySchema)
			if err != nil {
				return err
			}
			if !found || string(got) != want {
				t.Fatalf("source %q value=%q found=%v, want %q", source, got, found, want)
			}
			got[0] = 'X'
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.db.View(func(tx *bolt.Tx) error {
		if got := tx.Bucket(bucketMeta).Get(keySchema); !bytes.Equal(got, []byte("1")) {
			t.Fatalf("legacy schema version changed to %q", got)
		}
		if got := tx.Bucket(bucketMeta).Get(keyWriterID); !bytes.Equal(got, []byte(wantWriterID)) {
			t.Fatalf("legacy writer ID changed to %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir, "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.WriterID() != wantWriterID {
		t.Fatalf("writer ID changed after reopen: %q != %q", db.WriterID(), wantWriterID)
	}
	sourceState, err := db.SourceState("source-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceState.View(func(tx SourceStateReader) error {
		got, found, err := tx.Get(keySchema)
		if err != nil || !found || string(got) != "value-a" {
			t.Fatalf("persisted value=%q found=%v err=%v", got, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceStateUpdateIsAtomicAndSupportsIterationAndDelete(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sourceState, err := db.SourceState("source")
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("rollback")
	err = sourceState.Update(func(tx SourceStateWriter) error {
		if err := tx.Put([]byte("first"), []byte("one")); err != nil {
			return err
		}
		if err := tx.Put([]byte("second"), []byte("two")); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("SourceState.Update() error=%v, want %v", err, wantErr)
	}
	if err := sourceState.View(func(tx SourceStateReader) error {
		return tx.ForEach(func(key, value []byte) error {
			t.Fatalf("rolled-back entry remains: %q=%q", key, value)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := sourceState.Update(func(tx SourceStateWriter) error {
		if err := tx.Put([]byte("first"), []byte("one")); err != nil {
			return err
		}
		if err := tx.Put([]byte("second"), []byte("two")); err != nil {
			return err
		}
		return tx.Delete([]byte("first"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := sourceState.View(func(tx SourceStateReader) error {
		entries := make(map[string]string)
		if err := tx.ForEach(func(key, value []byte) error {
			entries[string(key)] = string(value)
			key[0] = 'X'
			value[0] = 'X'
			return nil
		}); err != nil {
			return err
		}
		if len(entries) != 1 || entries["second"] != "two" {
			t.Fatalf("entries=%v, want map[second:two]", entries)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sourceState.View(func(tx SourceStateReader) error {
		got, found, err := tx.Get([]byte("second"))
		if err != nil || !found || string(got) != "two" {
			t.Fatalf("value after copied iterator mutation=%q found=%v err=%v", got, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceStateRejectsInvalidNamesAndKeys(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.SourceState(""); err == nil {
		t.Fatal("SourceState() accepted an empty source name")
	}
	if _, err := db.SourceState("invalid/source"); err == nil {
		t.Fatal("SourceState() accepted a source name containing a slash")
	}
	if _, err := db.SourceState(string(bytes.Repeat([]byte{'s'}, bolt.MaxKeySize+1))); err == nil {
		t.Fatal("SourceState() accepted an oversized source name")
	}
	sourceState, err := db.SourceState("source")
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceState.View(nil); err == nil {
		t.Fatal("SourceState.View() accepted a nil callback")
	}
	if err := sourceState.Update(func(tx SourceStateWriter) error {
		if err := tx.Put(nil, []byte("value")); err == nil {
			t.Fatal("Put() accepted an empty key")
		}
		if err := tx.Delete(bytes.Repeat([]byte{'k'}, bolt.MaxKeySize+1)); err == nil {
			t.Fatal("Delete() accepted an oversized key")
		}
		if _, _, err := tx.Get(nil); err == nil {
			t.Fatal("Get() accepted an empty key")
		}
		if err := tx.ForEach(nil); err == nil {
			t.Fatal("ForEach() accepted a nil callback")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var nilState *SourceState
	if err := nilState.View(func(SourceStateReader) error { return nil }); err == nil {
		t.Fatal("nil SourceState.View() unexpectedly succeeded")
	}
}

func TestSourceStateRestrictsLegacyContainerLogCheckpoints(t *testing.T) {
	db, err := Open(t.TempDir(), "target", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	other, err := db.SourceState("other-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.LegacyContainerLogCheckpoint(); err == nil {
		t.Fatal("another Source accessed container-log checkpoints")
	}

	containerLogs, err := db.SourceState("container-logs")
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := containerLogs.LegacyContainerLogCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if streams, err := checkpoints.ListSourceStreams(); err != nil || len(streams) != 0 {
		t.Fatalf("ListSourceStreams() = %+v, error = %v", streams, err)
	}
}
