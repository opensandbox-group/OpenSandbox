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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
)

type registryImageDeleter interface {
	Delete(ctx context.Context, imageReference, imageDigest string, registrySecret *corev1.Secret, insecure bool) error
}

type remoteRegistryImageDeleter struct{}

func (remoteRegistryImageDeleter) Delete(
	ctx context.Context,
	imageReference string,
	imageDigest string,
	registrySecret *corev1.Secret,
	insecure bool,
) error {
	nameOptions := []name.Option{name.StrictValidation}
	if insecure {
		nameOptions = append(nameOptions, name.Insecure)
	}

	ref, err := name.ParseReference(imageReference, nameOptions...)
	if err != nil {
		return fmt.Errorf("parse image reference %q: %w", imageReference, err)
	}

	authenticator, err := registryAuthenticator(registrySecret, ref.Context().RegistryStr())
	if err != nil {
		return err
	}
	remoteOptions := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authenticator)}

	if imageDigest != "" {
		digestRef := ref.Context().Digest(imageDigest)
		if err := remote.Delete(digestRef, remoteOptions...); err == nil {
			return nil
		} else if !isRegistryNotFound(err) {
			return fmt.Errorf("delete image %q by digest %q: %w", imageReference, imageDigest, err)
		}
		// nerdctl can convert the pushed manifest media type, so the local
		// content-store digest may differ from the registry digest. Resolve the
		// tag after a digest miss and delete the registry's current manifest.
	}

	if _, ok := ref.(name.Digest); ok {
		if err := remote.Delete(ref, remoteOptions...); err != nil && !isRegistryNotFound(err) {
			return fmt.Errorf("delete image %q: %w", imageReference, err)
		}
		return nil
	}

	return deleteRegistryTag(ctx, ref, remoteOptions, imageReference)
}

func deleteRegistryTag(ctx context.Context, ref name.Reference, remoteOptions []remote.Option, imageReference string) error {
	descriptor, err := remote.Head(ref, remoteOptions...)
	if isRegistryNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve image digest for %q: %w", imageReference, err)
	}

	registryRef := ref.Context().Digest(descriptor.Digest.String())
	if err := remote.Delete(registryRef, remoteOptions...); err != nil && !isRegistryNotFound(err) {
		return fmt.Errorf("delete image %q by registry digest %q: %w", imageReference, descriptor.Digest, err)
	}
	return nil
}

func registryAuthenticator(secret *corev1.Secret, registry string) (authn.Authenticator, error) {
	if secret == nil {
		return authn.Anonymous, nil
	}

	var auths map[string]authn.AuthConfig
	var credentialHelpers map[string]string
	var credentialStore string
	switch {
	case len(secret.Data[corev1.DockerConfigJsonKey]) > 0:
		var config struct {
			Auths       map[string]authn.AuthConfig `json:"auths"`
			CredHelpers map[string]string           `json:"credHelpers"`
			CredsStore  string                      `json:"credsStore"`
		}
		if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config); err != nil {
			return nil, fmt.Errorf("parse registry secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		auths = config.Auths
		credentialHelpers = config.CredHelpers
		credentialStore = config.CredsStore
	case len(secret.Data[corev1.DockerConfigKey]) > 0:
		if err := json.Unmarshal(secret.Data[corev1.DockerConfigKey], &auths); err != nil {
			return nil, fmt.Errorf("parse registry secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
	default:
		return nil, fmt.Errorf("registry secret %s/%s has neither %s nor %s", secret.Namespace, secret.Name, corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
	}

	for server, config := range auths {
		if normalizeRegistry(server) == normalizeRegistry(registry) {
			return authn.FromConfig(config), nil
		}
	}
	for server, helper := range credentialHelpers {
		if normalizeRegistry(server) == normalizeRegistry(registry) {
			return nil, fmt.Errorf("registry secret %s/%s uses credential helper %q for %s; controller requires inline auths credentials", secret.Namespace, secret.Name, helper, registry)
		}
	}
	if credentialStore != "" && len(auths) == 0 {
		return nil, fmt.Errorf("registry secret %s/%s uses credential store %q; controller requires inline auths credentials", secret.Namespace, secret.Name, credentialStore)
	}
	return nil, fmt.Errorf("registry secret %s/%s has no credentials for %s", secret.Namespace, secret.Name, registry)
}

func normalizeRegistry(registry string) string {
	registry = strings.TrimPrefix(registry, "https://")
	registry = strings.TrimPrefix(registry, "http://")
	registry = strings.TrimSuffix(registry, "/")
	registry = strings.TrimSuffix(registry, "/v1")
	registry = strings.TrimSuffix(registry, "/v2")
	if registry == "index.docker.io" {
		return name.DefaultRegistry
	}
	return registry
}

func isRegistryNotFound(err error) bool {
	if err == nil {
		return false
	}
	var transportErr *transport.Error
	return errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound
}
