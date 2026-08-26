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

package registryimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCreatesLoadableImageLayoutWithDockerManifest(t *testing.T) {
	directory := t.TempDir()
	loaderPath := filepath.Join(directory, "loader")
	manifestPath := filepath.Join(directory, "manifest.json")
	payloadPath := filepath.Join(directory, "vmstate.zst")
	writeTestInput(t, loaderPath, []byte("loader"), 0755)
	writeTestInput(t, manifestPath, []byte(`{"formatVersion":"qemu-v1"}`), 0644)
	writeTestInput(t, payloadPath, []byte("compressed-memory"), 0644)
	archivePath := filepath.Join(directory, "image.tar")

	result, err := Build(archivePath, "registry.example/snapshots/vmstate:test", loaderPath, manifestPath, payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.ManifestDigest, "sha256:") {
		t.Fatalf("unexpected manifest digest %q", result.ManifestDigest)
	}

	entries := readArchive(t, archivePath)
	var index imageIndex
	if err := json.Unmarshal(entries["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != result.ManifestDigest {
		t.Fatalf("unexpected index: %#v", index)
	}
	if index.Manifests[0].Annotations[annotationReferenceName] != "registry.example/snapshots/vmstate:test" {
		t.Fatalf("image reference annotation is missing: %#v", index.Manifests[0].Annotations)
	}

	manifestData := entries[blobPath(result.ManifestDigest)]
	var manifest imageManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.MediaType != dockerManifestMediaType || len(manifest.Layers) != 1 {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	layerData := entries[blobPath(manifest.Layers[0].Digest)]
	gzipReader, err := gzip.NewReader(bytes.NewReader(layerData))
	if err != nil {
		t.Fatal(err)
	}
	layerEntries := make(map[string][]byte)
	layerReader := tar.NewReader(gzipReader)
	for {
		header, err := layerReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(layerReader)
		if err != nil {
			t.Fatal(err)
		}
		layerEntries[header.Name] = data
	}
	for _, name := range []string{
		"usr/local/bin/vmstate-loader",
		"opensandbox/checkpoint/manifest.json",
		"opensandbox/checkpoint/vmstate.zst",
	} {
		if len(layerEntries[name]) == 0 {
			t.Errorf("layer entry %q is missing", name)
		}
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entries := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = data
	}
	return entries
}

func writeTestInput(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
