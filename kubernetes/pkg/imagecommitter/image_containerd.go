// Copyright 2025 Alibaba Group Holding Ltd.
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
	"fmt"
	"time"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/rootfs"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ContainerdImageBuilder assembles OCI image content from writable snapshots.
type ContainerdImageBuilder struct {
	client            *containerd.Client
	sourceCredentials CredentialProvider
	sourceInsecure    InsecureRegistryFunc
}

// NewContainerdImageBuilder creates a builder with the credential and transport
// policies used to recover missing source-image content. The provider may
// return empty credentials for registries that permit anonymous pulls.
func NewContainerdImageBuilder(client *containerd.Client, sourceCredentials CredentialProvider, sourceInsecure InsecureRegistryFunc) *ContainerdImageBuilder {
	return &ContainerdImageBuilder{
		client:            client,
		sourceCredentials: sourceCredentials,
		sourceInsecure:    sourceInsecure,
	}
}

func (b *ContainerdImageBuilder) Commit(ctx context.Context, container ResolvedContainer, target string) (LocalImage, error) {
	if container.Snapshotter == "" || container.SnapshotKey == "" {
		return LocalImage{}, fmt.Errorf("container %s has no writable snapshot metadata", container.ID)
	}

	leaseCtx, done, err := b.client.WithLease(ctx, leases.WithRandomID(), leases.WithExpiration(time.Hour))
	if err != nil {
		return LocalImage{}, fmt.Errorf("create containerd lease: %w", err)
	}
	defer done(leaseCtx)

	c, err := b.client.LoadContainer(leaseCtx, container.ID)
	if err != nil {
		return LocalImage{}, fmt.Errorf("load container %s: %w", container.ID, err)
	}
	baseImage, err := c.Image(leaseCtx)
	if err != nil {
		return LocalImage{}, fmt.Errorf("load source image for container %s: %w", container.ID, err)
	}
	store := b.client.ContentStore()
	if err := b.ensureBaseImageContent(leaseCtx, baseImage); err != nil {
		return LocalImage{}, fmt.Errorf("recover source image content for container %s: %w", container.ID, err)
	}
	// Commit Jobs are pinned to the source sandbox's node and use that node's
	// containerd socket, so under the supported native execution model the
	// committer process platform matches the platform selected for the source
	// container. Cross-architecture emulation is not a supported snapshot mode.
	// ensureBaseImageContent intentionally uses the same platform matcher.
	baseManifest, err := images.Manifest(leaseCtx, store, baseImage.Target(), platforms.Default())
	if err != nil {
		return LocalImage{}, fmt.Errorf("read source manifest for container %s: %w", container.ID, err)
	}
	configData, err := content.ReadBlob(leaseCtx, store, baseManifest.Config)
	if err != nil {
		return LocalImage{}, fmt.Errorf("read source config for container %s: %w", container.ID, err)
	}
	var imageConfig ocispec.Image
	if err := json.Unmarshal(configData, &imageConfig); err != nil {
		return LocalImage{}, fmt.Errorf("decode source config for container %s: %w", container.ID, err)
	}

	mediaTypes := commitMediaTypes(baseManifest.MediaType)
	diffDesc, err := rootfs.CreateDiff(
		leaseCtx,
		container.SnapshotKey,
		b.client.SnapshotService(container.Snapshotter),
		b.client.DiffService(),
		diff.WithReference(fmt.Sprintf("opensandbox-commit-%s-%d", container.ID, time.Now().UnixNano())),
		diff.WithMediaType(mediaTypes.Diff),
	)
	if err != nil {
		return LocalImage{}, fmt.Errorf("create writable snapshot diff for container %s: %w", container.ID, err)
	}
	diffInfo, err := store.Info(leaseCtx, diffDesc.Digest)
	if err != nil {
		return LocalImage{}, fmt.Errorf("inspect diff content for container %s: %w", container.ID, err)
	}
	diffIDValue := diffInfo.Labels["containerd.io/uncompressed"]
	if diffIDValue == "" {
		return LocalImage{}, fmt.Errorf("diff for container %s has no uncompressed digest", container.ID)
	}
	diffID, err := digest.Parse(diffIDValue)
	if err != nil {
		return LocalImage{}, fmt.Errorf("parse diff ID for container %s: %w", container.ID, err)
	}
	// The comparer can report an OCI layer descriptor even when the source image
	// uses Docker media types. The bytes are compatible; the manifest descriptor
	// must use the selected image format.
	diffDesc.MediaType = mediaTypes.Layer

	now := time.Now().UTC()
	imageConfig.Created = &now
	imageConfig.RootFS.Type = "layers"
	imageConfig.RootFS.DiffIDs = append(imageConfig.RootFS.DiffIDs, diffID)
	imageConfig.History = append(imageConfig.History, ocispec.History{
		Created:   &now,
		CreatedBy: "OpenSandbox image committer",
	})
	newConfigData, err := json.Marshal(imageConfig)
	if err != nil {
		return LocalImage{}, fmt.Errorf("encode committed image config: %w", err)
	}
	configDesc := ocispec.Descriptor{
		MediaType: mediaTypes.Config,
		Digest:    digest.FromBytes(newConfigData),
		Size:      int64(len(newConfigData)),
	}
	if err := content.WriteBlob(
		leaseCtx,
		store,
		"opensandbox-config-"+configDesc.Digest.String(),
		bytes.NewReader(newConfigData),
		configDesc,
	); err != nil {
		return LocalImage{}, fmt.Errorf("write committed image config: %w", err)
	}

	layers := append([]ocispec.Descriptor(nil), baseManifest.Layers...)
	layers = append(layers, diffDesc)
	newManifest := ocispec.Manifest{
		Versioned:    baseManifest.Versioned,
		MediaType:    mediaTypes.Manifest,
		ArtifactType: baseManifest.ArtifactType,
		Config:       configDesc,
		Layers:       layers,
		Subject:      baseManifest.Subject,
		Annotations:  baseManifest.Annotations,
	}
	newManifestData, err := json.Marshal(newManifest)
	if err != nil {
		return LocalImage{}, fmt.Errorf("encode committed image manifest: %w", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: mediaTypes.Manifest,
		Digest:    digest.FromBytes(newManifestData),
		Size:      int64(len(newManifestData)),
	}
	gcLabels := map[string]string{"containerd.io/gc.ref.content.0": configDesc.Digest.String()}
	for i, layer := range layers {
		gcLabels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i+1)] = layer.Digest.String()
	}
	if err := content.WriteBlob(
		leaseCtx,
		store,
		"opensandbox-manifest-"+manifestDesc.Digest.String(),
		bytes.NewReader(newManifestData),
		manifestDesc,
		content.WithLabels(gcLabels),
	); err != nil {
		return LocalImage{}, fmt.Errorf("write committed image manifest: %w", err)
	}

	imageRecord := images.Image{Name: target, Target: manifestDesc, CreatedAt: now, UpdatedAt: now}
	if _, err := b.client.ImageService().Update(leaseCtx, imageRecord); err != nil {
		if !errdefs.IsNotFound(err) {
			return LocalImage{}, fmt.Errorf("update target image %s: %w", target, err)
		}
		if _, err := b.client.ImageService().Create(leaseCtx, imageRecord); err != nil {
			return LocalImage{}, fmt.Errorf("create target image %s: %w", target, err)
		}
	}

	return LocalImage{Reference: target, Target: manifestDesc, Config: configDesc}, nil
}

func (b *ContainerdImageBuilder) ensureBaseImageContent(ctx context.Context, image containerd.Image) error {
	return ensurePlatformContent(
		ctx,
		b.client.ContentStore(),
		image.Target(),
		platforms.Default(),
		func(ctx context.Context) error {
			return b.fetchBaseImageContent(ctx, image)
		},
	)
}

func (b *ContainerdImageBuilder) fetchBaseImageContent(ctx context.Context, image containerd.Image) error {
	sourceReference, err := referenceWithDigest(image.Name(), image.Target())
	if err != nil {
		return err
	}
	host, err := registryHost(sourceReference)
	if err != nil {
		return err
	}
	credential := RegistryCredential{}
	if b.sourceCredentials != nil {
		credential, err = b.sourceCredentials.Credential(ctx, host)
		if err != nil {
			return fmt.Errorf("resolve source credentials for %s: %w", host, err)
		}
	}
	insecure := b.sourceInsecure != nil && b.sourceInsecure(sourceReference)
	if err := b.fetchBaseImageContentWithTransport(ctx, sourceReference, host, credential, "https", insecure); err != nil {
		if !insecure || !shouldFallbackToPlainHTTP(err) {
			return fmt.Errorf("fetch source image %s: %w", sourceReference, err)
		}
		if err := b.fetchBaseImageContentWithTransport(ctx, sourceReference, host, credential, "http", false); err != nil {
			return fmt.Errorf("fetch source image %s over plain HTTP: %w", sourceReference, err)
		}
	}
	return nil
}

func (b *ContainerdImageBuilder) fetchBaseImageContentWithTransport(
	ctx context.Context,
	sourceReference string,
	host string,
	credential RegistryCredential,
	scheme string,
	skipVerify bool,
) error {
	resolver := newDockerResolver(host, credential, scheme, skipVerify)
	_, err := b.client.Fetch(
		ctx,
		sourceReference,
		containerd.WithResolver(resolver),
		containerd.WithPlatform(platforms.DefaultString()),
	)
	return err
}

func ensurePlatformContent(
	ctx context.Context,
	store content.Store,
	target ocispec.Descriptor,
	platform platforms.MatchComparer,
	fetch func(context.Context) error,
) error {
	_, _, _, missing, err := images.Check(ctx, store, target, platform)
	if err != nil {
		return fmt.Errorf("check source image content: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	if err := fetch(ctx); err != nil {
		return fmt.Errorf("fetch %d missing source image blob(s): %w", len(missing), err)
	}
	_, _, _, missing, err = images.Check(ctx, store, target, platform)
	if err != nil {
		return fmt.Errorf("recheck source image content: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("source image still has %d missing blob(s) after fetch", len(missing))
	}
	return nil
}

type commitMediaTypeSet struct {
	Manifest string
	Config   string
	Layer    string
	Diff     string
}

func commitMediaTypes(baseManifestMediaType string) commitMediaTypeSet {
	// containerd's diff service accepts the OCI compression media type. Docker
	// schema 2 uses the same gzip bytes with a different descriptor media type,
	// so request an OCI diff and relabel the resulting descriptor when writing a
	// Docker manifest.
	if baseManifestMediaType == images.MediaTypeDockerSchema2Manifest {
		return commitMediaTypeSet{
			Manifest: images.MediaTypeDockerSchema2Manifest,
			Config:   images.MediaTypeDockerSchema2Config,
			Layer:    images.MediaTypeDockerSchema2LayerGzip,
			Diff:     ocispec.MediaTypeImageLayerGzip,
		}
	}
	return commitMediaTypeSet{
		Manifest: ocispec.MediaTypeImageManifest,
		Config:   ocispec.MediaTypeImageConfig,
		Layer:    ocispec.MediaTypeImageLayerGzip,
		Diff:     ocispec.MediaTypeImageLayerGzip,
	}
}
