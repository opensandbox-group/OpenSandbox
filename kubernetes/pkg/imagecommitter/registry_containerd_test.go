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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestDockerConfigCredentialProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("robot:secret"))
	data := []byte(`{"auths":{"https://registry.example.com/v1/":{"auth":"` + auth + `"}}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	provider := DockerConfigCredentialProvider{Path: path}
	credential, err := provider.Credential(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("Credential failed: %v", err)
	}
	if credential.Username != "robot" || credential.Password != "secret" {
		t.Fatalf("unexpected credential: %#v", credential)
	}
	other, err := provider.Credential(context.Background(), "other.example.com")
	if err != nil {
		t.Fatalf("Credential for other host failed: %v", err)
	}
	if other != (RegistryCredential{}) {
		t.Fatalf("credential leaked to another host: %#v", other)
	}
}

func TestDockerConfigCredentialProviderFallsBackToAnonymousOnInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`not-json`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var warnings bytes.Buffer
	credential, err := (DockerConfigCredentialProvider{Path: path, ErrorOutput: &warnings}).Credential(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("invalid config should remain best effort: %v", err)
	}
	if credential != (RegistryCredential{}) {
		t.Fatalf("invalid config returned credential: %#v", credential)
	}
	if warnings.Len() == 0 {
		t.Fatal("expected invalid config warning")
	}
}

func TestDockerConfigCredentialProviderSupportsIdentityToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auths":{"registry.example.com":{"identitytoken":"token"}}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	credential, err := (DockerConfigCredentialProvider{Path: path}).Credential(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("Credential failed: %v", err)
	}
	if credential.RefreshToken != "token" {
		t.Fatalf("refresh token = %q", credential.RefreshToken)
	}
}

func TestShouldFallbackToPlainHTTP(t *testing.T) {
	if !shouldFallbackToPlainHTTP(http.ErrSchemeMismatch) {
		t.Fatal("scheme mismatch should fall back to HTTP")
	}
	if !shouldFallbackToPlainHTTP(fmt.Errorf("connect: %w", syscall.ECONNREFUSED)) {
		t.Fatal("connection refused should fall back to HTTP")
	}
	if shouldFallbackToPlainHTTP(errors.New("unauthorized")) {
		t.Fatal("authentication errors must not fall back to HTTP")
	}
}

func TestNormalizeRegistryHostAliasesDockerHub(t *testing.T) {
	for _, host := range []string{"docker.io", "registry-1.docker.io", "https://index.docker.io/v1/"} {
		if got := normalizeRegistryHost(host); got != "docker.io" {
			t.Fatalf("normalizeRegistryHost(%q) = %q", host, got)
		}
	}
}

func TestRegistryHost(t *testing.T) {
	host, err := registryHost("registry.example.com:5000/project/image:snap")
	if err != nil {
		t.Fatalf("registryHost failed: %v", err)
	}
	if host != "registry.example.com:5000" {
		t.Fatalf("host = %q", host)
	}
}

func TestReferenceWithDigestReplacesMutableTag(t *testing.T) {
	descriptor := ocispec.Descriptor{Digest: digest.FromString("source image")}
	got, err := referenceWithDigest("registry.example.com/project/image:latest", descriptor)
	if err != nil {
		t.Fatalf("referenceWithDigest failed: %v", err)
	}
	want := "registry.example.com/project/image@" + descriptor.Digest.String()
	if got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
}
