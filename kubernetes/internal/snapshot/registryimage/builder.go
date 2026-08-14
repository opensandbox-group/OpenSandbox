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
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

const (
	ociImageIndexMediaType       = "application/vnd.oci.image.index.v1+json"
	dockerManifestMediaType      = "application/vnd.docker.distribution.manifest.v2+json"
	dockerConfigMediaType        = "application/vnd.docker.container.image.v1+json"
	dockerCompressedLayerType    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	annotationReferenceName      = "org.opencontainers.image.ref.name"
	annotationContainerdImageRef = "io.containerd.image.name"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type imageConfig struct {
	Created      time.Time `json:"created"`
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	Config       struct {
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	History []struct {
		Created   time.Time `json:"created"`
		CreatedBy string    `json:"created_by"`
	} `json:"history"`
}

// BuildResult describes the locally constructed registry manifest.
type BuildResult struct {
	ManifestDigest string
}

// Build creates an OCI image-layout archive whose image manifest uses Docker
// schema2 media types. That combination is loadable by containerd/nerdctl and
// accepted by registries that do not implement OCI artifact media types.
func Build(archivePath, imageRef, loaderPath, manifestPath, payloadPath string) (BuildResult, error) {
	if imageRef == "" {
		return BuildResult{}, fmt.Errorf("VM state image reference is required")
	}
	workDir, err := os.MkdirTemp(filepath.Dir(archivePath), ".opensandbox-vmstate-image-")
	if err != nil {
		return BuildResult{}, fmt.Errorf("create image build directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	layerPath := filepath.Join(workDir, "layer.tar.gz")
	layerDescriptor, diffID, err := buildLayer(layerPath, loaderPath, manifestPath, payloadPath)
	if err != nil {
		return BuildResult{}, err
	}
	now := time.Now().UTC()
	config := imageConfig{Created: now, Architecture: runtime.GOARCH, OS: "linux"}
	config.Config.Entrypoint = []string{"/usr/local/bin/vmstate-loader"}
	config.Config.Cmd = []string{"verify"}
	config.Config.Labels = map[string]string{
		"io.opensandbox.snapshot.format": snapshot.VMStateFormatVersion1,
	}
	config.RootFS.Type = "layers"
	config.RootFS.DiffIDs = []string{diffID}
	config.History = append(config.History, struct {
		Created   time.Time `json:"created"`
		CreatedBy string    `json:"created_by"`
	}{Created: now, CreatedBy: "OpenSandbox QEMU VMState checkpoint"})
	configData, err := json.Marshal(config)
	if err != nil {
		return BuildResult{}, fmt.Errorf("marshal VM state image config: %w", err)
	}
	configDescriptor := bytesDescriptor(dockerConfigMediaType, configData)

	manifest := imageManifest{
		SchemaVersion: 2,
		MediaType:     dockerManifestMediaType,
		Config:        configDescriptor,
		Layers:        []descriptor{layerDescriptor},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return BuildResult{}, fmt.Errorf("marshal VM state image manifest: %w", err)
	}
	manifestDescriptor := bytesDescriptor(dockerManifestMediaType, manifestData)
	manifestDescriptor.Annotations = map[string]string{
		annotationReferenceName:      imageRef,
		annotationContainerdImageRef: imageRef,
	}
	indexData, err := json.Marshal(imageIndex{
		SchemaVersion: 2,
		MediaType:     ociImageIndexMediaType,
		Manifests:     []descriptor{manifestDescriptor},
	})
	if err != nil {
		return BuildResult{}, fmt.Errorf("marshal VM state image index: %w", err)
	}

	archive, err := os.Create(archivePath)
	if err != nil {
		return BuildResult{}, fmt.Errorf("create VM state image archive: %w", err)
	}
	archiveWriter := tar.NewWriter(archive)
	writeErr := writeImageLayout(archiveWriter, indexData, configDescriptor, configData, manifestDescriptor, manifestData, layerDescriptor, layerPath)
	if closeErr := archiveWriter.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if closeErr := archive.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return BuildResult{}, fmt.Errorf("write VM state image archive: %w", writeErr)
	}
	return BuildResult{ManifestDigest: manifestDescriptor.Digest}, nil
}

func buildLayer(path, loaderPath, manifestPath, payloadPath string) (descriptor, string, error) {
	layer, err := os.Create(path)
	if err != nil {
		return descriptor{}, "", err
	}
	compressedHash := sha256.New()
	gzipWriter, err := gzip.NewWriterLevel(io.MultiWriter(layer, compressedHash), gzip.BestSpeed)
	if err != nil {
		layer.Close()
		return descriptor{}, "", err
	}
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	diffHash := sha256.New()
	tarWriter := tar.NewWriter(io.MultiWriter(gzipWriter, diffHash))

	entries := []struct {
		source string
		target string
		mode   int64
	}{
		{loaderPath, "usr/local/bin/" + snapshot.VMStateLoaderFilename, 0755},
		{manifestPath, "opensandbox/checkpoint/" + snapshot.VMStateManifestFilename, 0644},
		{payloadPath, "opensandbox/checkpoint/" + snapshot.VMStatePayloadFilename, 0644},
	}
	for _, directory := range []string{"usr/", "usr/local/", "usr/local/bin/", "opensandbox/", "opensandbox/checkpoint/"} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: directory, Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Unix(0, 0)}); err != nil {
			return descriptor{}, "", closeLayerWriters(layer, gzipWriter, tarWriter, err)
		}
	}
	for _, entry := range entries {
		if err := addLayerFile(tarWriter, entry.source, entry.target, entry.mode); err != nil {
			return descriptor{}, "", closeLayerWriters(layer, gzipWriter, tarWriter, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return descriptor{}, "", closeLayerWriters(layer, gzipWriter, nil, err)
	}
	if err := gzipWriter.Close(); err != nil {
		layer.Close()
		return descriptor{}, "", err
	}
	if err := layer.Close(); err != nil {
		return descriptor{}, "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return descriptor{}, "", err
	}
	return descriptor{
		MediaType: dockerCompressedLayerType,
		Digest:    hashDigest(compressedHash),
		Size:      info.Size(),
	}, hashDigest(diffHash), nil
}

func closeLayerWriters(layer *os.File, gzipWriter *gzip.Writer, tarWriter *tar.Writer, cause error) error {
	if tarWriter != nil {
		_ = tarWriter.Close()
	}
	_ = gzipWriter.Close()
	_ = layer.Close()
	return cause
}

func addLayerFile(writer *tar.Writer, sourcePath, targetPath string, mode int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open image layer input %q: %w", sourcePath, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("image layer input %q is not a regular file", sourcePath)
	}
	header := &tar.Header{Name: targetPath, Mode: mode, Size: info.Size(), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(writer, source)
	return err
}

func writeImageLayout(
	writer *tar.Writer,
	indexData []byte,
	configDescriptor descriptor,
	configData []byte,
	manifestDescriptor descriptor,
	manifestData []byte,
	layerDescriptor descriptor,
	layerPath string,
) error {
	if err := writeBytes(writer, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := writeBytes(writer, "index.json", indexData); err != nil {
		return err
	}
	if err := writeBytes(writer, blobPath(configDescriptor.Digest), configData); err != nil {
		return err
	}
	if err := writeBytes(writer, blobPath(manifestDescriptor.Digest), manifestData); err != nil {
		return err
	}
	return writeFile(writer, blobPath(layerDescriptor.Digest), layerPath, layerDescriptor.Size)
}

func writeBytes(writer *tar.Writer, name string, data []byte) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeFile(writer *tar.Writer, name, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: size, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}

func bytesDescriptor(mediaType string, data []byte) descriptor {
	sum := sha256.Sum256(data)
	return descriptor{MediaType: mediaType, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

func hashDigest(value hash.Hash) string {
	return "sha256:" + hex.EncodeToString(value.Sum(nil))
}

func blobPath(digest string) string {
	return "blobs/sha256/" + digest[len("sha256:"):]
}
