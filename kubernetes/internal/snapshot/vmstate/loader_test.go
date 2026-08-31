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

package vmstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
	"github.com/klauspost/compress/zstd"
)

func TestRestoreAndStream(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	plain := []byte("guest-memory-token")
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	payload := compressed.Bytes()
	payloadDigest := sha256.Sum256(payload)
	manifest := snapshot.VMStateManifest{
		FormatVersion: snapshot.VMStateFormatVersion1,
		PayloadDigest: "sha256:" + hex.EncodeToString(payloadDigest[:]),
		PayloadSize:   int64(len(payload)),
		Compression:   snapshot.VMStateCompressionZstd,
		Compatibility: snapshot.QEMUCompatibility{
			Architecture:     "x86_64",
			QEMUVersion:      "9.1.0",
			MachineType:      "pc-q35-9.1",
			CPUModel:         "host",
			VCPUs:            2,
			MemoryBytes:      1 << 30,
			QEMUConfigDigest: "sha256:config",
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestData)
	writeTestFile(t, filepath.Join(source, snapshot.VMStateManifestFilename), manifestData, 0640)
	writeTestFile(t, filepath.Join(source, snapshot.VMStatePayloadFilename), payload, 0640)
	loaderPath := filepath.Join(source, snapshot.VMStateLoaderFilename)
	writeTestFile(t, loaderPath, []byte("loader"), 0750)

	if err := Restore(source, target, "sha256:"+hex.EncodeToString(manifestDigest[:]), loaderPath); err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err := Stream(
		filepath.Join(target, snapshot.VMStateManifestFilename),
		filepath.Join(target, snapshot.VMStatePayloadFilename),
		&restored,
	); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), plain) {
		t.Fatalf("unexpected restored stream %q", restored.Bytes())
	}
}

func TestVerifyPayloadRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmstate.zst")
	writeTestFile(t, path, []byte("corrupt"), 0640)
	manifest := snapshot.VMStateManifest{
		PayloadDigest: "sha256:" + strings.Repeat("0", 64),
		PayloadSize:   7,
	}
	if err := VerifyPayload(path, manifest); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
