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
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DockerConfigCredentialProvider reads the Kubernetes dockerconfigjson mount.
type DockerConfigCredentialProvider struct {
	Path        string
	ErrorOutput io.Writer
}

type dockerConfig struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

func (p DockerConfigCredentialProvider) Credential(_ context.Context, registryHost string) (RegistryCredential, error) {
	if p.Path == "" {
		return RegistryCredential{}, nil
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.warn("read registry credential config: %v", err)
		}
		return RegistryCredential{}, nil
	}
	var config dockerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		p.warn("parse registry credential config: %v", err)
		return RegistryCredential{}, nil
	}
	for configuredHost, entry := range config.Auths {
		if normalizeRegistryHost(configuredHost) != normalizeRegistryHost(registryHost) {
			continue
		}
		credential := RegistryCredential{
			Username:     entry.Username,
			Password:     entry.Password,
			AccessToken:  entry.RegistryToken,
			RefreshToken: entry.IdentityToken,
		}
		if entry.Auth != "" && credential.Username == "" && credential.Password == "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				p.warn("decode registry auth for %s: %v", registryHost, err)
				return RegistryCredential{}, nil
			}
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) != 2 {
				p.warn("invalid registry auth for %s", registryHost)
				return RegistryCredential{}, nil
			}
			credential.Username, credential.Password = parts[0], parts[1]
		}
		return credential, nil
	}
	p.warn("no registry credential found for %s; attempting registry access without credentials", registryHost)
	return RegistryCredential{}, nil
}

func (p DockerConfigCredentialProvider) warn(format string, args ...any) {
	if p.ErrorOutput != nil {
		fmt.Fprintf(p.ErrorOutput, "WARNING: "+format+"\n", args...)
	}
}

// InsecureRegistryFunc reports whether a registry reference may skip TLS
// verification and fall back to plain HTTP.
type InsecureRegistryFunc func(imageReference string) bool

// ContainerdImagePusher pushes image content through containerd's resolver.
type ContainerdImagePusher struct {
	client      *containerd.Client
	credentials CredentialProvider
	insecure    InsecureRegistryFunc
}

func NewContainerdImagePusher(client *containerd.Client, credentials CredentialProvider, insecure InsecureRegistryFunc) *ContainerdImagePusher {
	return &ContainerdImagePusher{client: client, credentials: credentials, insecure: insecure}
}

func (p *ContainerdImagePusher) Push(ctx context.Context, image LocalImage) (ocispec.Descriptor, error) {
	host, err := registryHost(image.Reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	credential := RegistryCredential{}
	if p.credentials != nil {
		credential, err = p.credentials.Credential(ctx, host)
		if err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("resolve credentials for %s: %w", host, err)
		}
	}
	insecure := p.insecure != nil && p.insecure(image.Reference)
	if err := p.push(ctx, image, host, credential, "https", insecure); err != nil {
		if !insecure || !shouldFallbackToPlainHTTP(err) {
			return ocispec.Descriptor{}, fmt.Errorf("push image %s: %w", image.Reference, err)
		}
		if err := p.push(ctx, image, host, credential, "http", false); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("push image %s over plain HTTP: %w", image.Reference, err)
		}
	}
	return image.Target, nil
}

func (p *ContainerdImagePusher) push(ctx context.Context, image LocalImage, host string, credential RegistryCredential, scheme string, skipVerify bool) error {
	resolver := newDockerResolver(host, credential, scheme, skipVerify)
	return p.client.Push(ctx, image.Reference, image.Target, containerd.WithResolver(resolver))
}

func newDockerResolver(host string, credential RegistryCredential, scheme string, skipVerify bool) remotes.Resolver {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if skipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicitly configured insecure registry
	}
	client := &http.Client{Transport: transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && (req.URL.Scheme != via[len(via)-1].URL.Scheme || req.URL.Host != via[len(via)-1].URL.Host) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	headers := http.Header{}
	if credential.AccessToken != "" {
		headers.Set("Authorization", "Bearer "+credential.AccessToken)
	}
	credentials := func(requestedHost string) (string, string, error) {
		if normalizeRegistryHost(requestedHost) != normalizeRegistryHost(host) {
			return "", "", nil
		}
		if credential.AccessToken != "" {
			return "", "", nil
		}
		if credential.RefreshToken != "" {
			return "", credential.RefreshToken, nil
		}
		return credential.Username, credential.Password, nil
	}
	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthClient(client),
		docker.WithAuthHeader(headers),
		docker.WithAuthCreds(credentials),
	)
	return docker.NewResolver(docker.ResolverOptions{Hosts: func(requestedHost string) ([]docker.RegistryHost, error) {
		actualHost := requestedHost
		if requestedHost == "docker.io" {
			actualHost = "registry-1.docker.io"
		}
		return []docker.RegistryHost{{
			Client:       client,
			Authorizer:   authorizer,
			Host:         actualHost,
			Scheme:       scheme,
			Path:         "/v2",
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityPush,
			Header:       headers.Clone(),
		}}, nil
	}})
}

func shouldFallbackToPlainHTTP(err error) bool {
	return errors.Is(err, http.ErrSchemeMismatch) || errors.Is(err, syscall.ECONNREFUSED)
}

func registryHost(imageReference string) (string, error) {
	named, err := reference.ParseNormalizedNamed(imageReference)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", imageReference, err)
	}
	return reference.Domain(named), nil
}

func referenceWithDigest(imageReference string, descriptor ocispec.Descriptor) (string, error) {
	named, err := reference.ParseNormalizedNamed(imageReference)
	if err != nil {
		return "", fmt.Errorf("parse source image %q: %w", imageReference, err)
	}
	canonical, err := reference.WithDigest(reference.TrimNamed(named), descriptor.Digest)
	if err != nil {
		return "", fmt.Errorf("pin source image %q to digest %s: %w", imageReference, descriptor.Digest, err)
	}
	return canonical.String(), nil
}

func normalizeRegistryHost(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimSuffix(value, "/v1/")
	value = strings.TrimSuffix(value, "/v2/")
	value = strings.TrimSuffix(value, "/")
	switch value {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	default:
		return value
	}
}
