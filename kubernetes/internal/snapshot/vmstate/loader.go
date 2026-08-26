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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

const fileMode = 0640

func ReadManifest(path string, expectedDigest string) (snapshot.VMStateManifest, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot.VMStateManifest{}, "", fmt.Errorf("read VM state manifest: %w", err)
	}
	digest := digestBytes(data)
	if expectedDigest != "" && digest != expectedDigest {
		return snapshot.VMStateManifest{}, digest, fmt.Errorf("VM state manifest digest mismatch: expected %s, got %s", expectedDigest, digest)
	}
	var manifest snapshot.VMStateManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return snapshot.VMStateManifest{}, digest, fmt.Errorf("decode VM state manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return snapshot.VMStateManifest{}, digest, err
	}
	return manifest, digest, nil
}

func VerifyPayload(path string, manifest snapshot.VMStateManifest) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open VM state payload: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("hash VM state payload: %w", err)
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if size != manifest.PayloadSize {
		return fmt.Errorf("VM state payload size mismatch: expected %d, got %d", manifest.PayloadSize, size)
	}
	if actualDigest != manifest.PayloadDigest {
		return fmt.Errorf("VM state payload digest mismatch: expected %s, got %s", manifest.PayloadDigest, actualDigest)
	}
	return nil
}

// Restore verifies an immutable checkpoint image and copies its fixed files
// into the shared emptyDir. The manifest is renamed last and acts as the ready
// marker consumed by the QEMU entrypoint.
func Restore(sourceDir, targetDir, expectedManifestDigest, executablePath string) error {
	manifestPath := filepath.Join(sourceDir, snapshot.VMStateManifestFilename)
	manifest, _, err := ReadManifest(manifestPath, expectedManifestDigest)
	if err != nil {
		return err
	}
	payloadPath := filepath.Join(sourceDir, snapshot.VMStatePayloadFilename)
	if err := VerifyPayload(payloadPath, manifest); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		return fmt.Errorf("create VM state restore directory: %w", err)
	}

	files := []struct {
		source string
		target string
		mode   os.FileMode
	}{
		{source: payloadPath, target: filepath.Join(targetDir, snapshot.VMStatePayloadFilename), mode: fileMode},
		{source: executablePath, target: filepath.Join(targetDir, snapshot.VMStateLoaderFilename), mode: 0750},
		{source: manifestPath, target: filepath.Join(targetDir, snapshot.VMStateManifestFilename), mode: fileMode},
	}
	for _, file := range files {
		if err := copyAtomically(file.source, file.target, file.mode); err != nil {
			return err
		}
	}
	return nil
}

// Stream verifies and decompresses a migration stream to output. It is used
// by QEMU's -incoming exec: transport in the restored container.
func Stream(manifestPath, payloadPath string, output io.Writer) error {
	manifest, _, err := ReadManifest(manifestPath, "")
	if err != nil {
		return err
	}
	if err := VerifyPayload(payloadPath, manifest); err != nil {
		return err
	}
	payload, err := os.Open(payloadPath)
	if err != nil {
		return fmt.Errorf("open VM state payload: %w", err)
	}
	defer payload.Close()
	decoder, err := zstd.NewReader(payload)
	if err != nil {
		return fmt.Errorf("open zstd VM state stream: %w", err)
	}
	defer decoder.Close()
	if _, err := io.Copy(output, decoder); err != nil {
		return fmt.Errorf("decompress VM state stream: %w", err)
	}
	return nil
}

func copyAtomically(sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %q: %w", sourcePath, err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".opensandbox-restore-*")
	if err != nil {
		return fmt.Errorf("create temporary restore file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return fmt.Errorf("copy %q: %w", sourcePath, err)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set mode on %q: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync %q: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %q: %w", temporaryPath, err)
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return fmt.Errorf("publish %q: %w", targetPath, err)
	}
	return nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
