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

package imagecommitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCommitMediaTypesUsesOCIDiffForDockerManifest(t *testing.T) {
	mediaTypes := commitMediaTypes(images.MediaTypeDockerSchema2Manifest)
	if mediaTypes.Manifest != images.MediaTypeDockerSchema2Manifest {
		t.Fatalf("manifest media type = %q", mediaTypes.Manifest)
	}
	if mediaTypes.Config != images.MediaTypeDockerSchema2Config {
		t.Fatalf("config media type = %q", mediaTypes.Config)
	}
	if mediaTypes.Layer != images.MediaTypeDockerSchema2LayerGzip {
		t.Fatalf("manifest layer media type = %q", mediaTypes.Layer)
	}
	if mediaTypes.Diff != ocispec.MediaTypeImageLayerGzip {
		t.Fatalf("diff service media type = %q, want OCI gzip", mediaTypes.Diff)
	}
}

func TestCommitMediaTypesUsesOCIForOCIManifest(t *testing.T) {
	mediaTypes := commitMediaTypes(ocispec.MediaTypeImageManifest)
	if mediaTypes.Manifest != ocispec.MediaTypeImageManifest ||
		mediaTypes.Config != ocispec.MediaTypeImageConfig ||
		mediaTypes.Layer != ocispec.MediaTypeImageLayerGzip ||
		mediaTypes.Diff != ocispec.MediaTypeImageLayerGzip {
		t.Fatalf("unexpected OCI media types: %#v", mediaTypes)
	}
}

func TestEnsurePlatformContentFetchesMissingSelectedPlatformBlob(t *testing.T) {
	store, target, missingLayer := incompleteMultiPlatformImage(t)
	fetches := 0
	err := ensurePlatformContent(
		context.Background(),
		store,
		target,
		platforms.Only(ocispec.Platform{OS: "linux", Architecture: "amd64"}),
		func(ctx context.Context) error {
			fetches++
			return writeTestBlob(ctx, store, missingLayer, []byte("selected-platform-layer"))
		},
	)
	if err != nil {
		t.Fatalf("ensurePlatformContent failed: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
}

func TestEnsurePlatformContentSkipsFetchWhenSelectedPlatformIsComplete(t *testing.T) {
	store, target, missingLayer := incompleteMultiPlatformImage(t)
	if err := writeTestBlob(context.Background(), store, missingLayer, []byte("selected-platform-layer")); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	err := ensurePlatformContent(
		context.Background(),
		store,
		target,
		platforms.Only(ocispec.Platform{OS: "linux", Architecture: "amd64"}),
		func(context.Context) error {
			fetches++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ensurePlatformContent failed: %v", err)
	}
	if fetches != 0 {
		t.Fatalf("fetches = %d, want 0", fetches)
	}
}

func TestEnsurePlatformContentReportsFetchFailure(t *testing.T) {
	store, target, _ := incompleteMultiPlatformImage(t)
	err := ensurePlatformContent(
		context.Background(),
		store,
		target,
		platforms.Only(ocispec.Platform{OS: "linux", Architecture: "amd64"}),
		func(context.Context) error { return errors.New("registry unavailable") },
	)
	if err == nil || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("error = %v, want registry failure", err)
	}
}

func TestEnsurePlatformContentRejectsIncompleteFetch(t *testing.T) {
	store, target, _ := incompleteMultiPlatformImage(t)
	err := ensurePlatformContent(
		context.Background(),
		store,
		target,
		platforms.Only(ocispec.Platform{OS: "linux", Architecture: "amd64"}),
		func(context.Context) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "still has 1 missing blob") {
		t.Fatalf("error = %v, want incomplete fetch failure", err)
	}
}

func incompleteMultiPlatformImage(t *testing.T) (content.Store, ocispec.Descriptor, ocispec.Descriptor) {
	t.Helper()
	ctx := context.Background()
	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := descriptorFor(ocispec.MediaTypeImageConfig, configData)
	if err := writeTestBlob(ctx, store, config, configData); err != nil {
		t.Fatal(err)
	}
	layerData := []byte("selected-platform-layer")
	layer := descriptorFor(ocispec.MediaTypeImageLayerGzip, layerData)
	manifestData, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := descriptorFor(ocispec.MediaTypeImageManifest, manifestData)
	manifest.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	if err := writeTestBlob(ctx, store, manifest, manifestData); err != nil {
		t.Fatal(err)
	}
	foreignManifest := descriptorFor(ocispec.MediaTypeImageManifest, []byte("absent-arm64-manifest"))
	foreignManifest.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64"}
	indexData, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifest, foreignManifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	index := descriptorFor(ocispec.MediaTypeImageIndex, indexData)
	if err := writeTestBlob(ctx, store, index, indexData); err != nil {
		t.Fatal(err)
	}
	return store, index, layer
}

func descriptorFor(mediaType string, data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

func writeTestBlob(ctx context.Context, store content.Store, descriptor ocispec.Descriptor, data []byte) error {
	return content.WriteBlob(ctx, store, "test-"+descriptor.Digest.String(), bytes.NewReader(data), descriptor)
}
