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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRemoteRegistryImageDeleter_ResolvesTagAndDeletesDigest(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deletedPath := make(chan string, 1)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead && request.URL.Path == "/v2/snapshots/test/manifests/tag":
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete:
			deletedPath <- request.URL.Path
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, request)
		}
	}))
	defer registry.Close()

	imageReference := strings.TrimPrefix(registry.URL, "http://") + "/snapshots/test:tag"
	err := (remoteRegistryImageDeleter{}).Delete(context.Background(), imageReference, "", nil, true)
	require.NoError(t, err)
	select {
	case path := <-deletedPath:
		assert.Equal(t, "/v2/snapshots/test/manifests/"+digest, path)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for registry DELETE request")
	}
}

func TestRemoteRegistryImageDeleter_TreatsMissingManifestAsSuccess(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, request)
	}))
	defer registry.Close()

	imageReference := strings.TrimPrefix(registry.URL, "http://") + "/snapshots/missing@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err := (remoteRegistryImageDeleter{}).Delete(context.Background(), imageReference, "", nil, true)
	require.NoError(t, err)
}

func TestRemoteRegistryImageDeleter_FallsBackToTagAfterRecordedDigestMiss(t *testing.T) {
	const recordedDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const registryDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deletedPaths := make(chan string, 2)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodHead && request.URL.Path == "/v2/snapshots/test/manifests/tag":
			w.Header().Set("Docker-Content-Digest", registryDigest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/"+recordedDigest):
			deletedPaths <- request.URL.Path
			http.NotFound(w, request)
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/"+registryDigest):
			deletedPaths <- request.URL.Path
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, request)
		}
	}))
	defer registry.Close()

	imageReference := strings.TrimPrefix(registry.URL, "http://") + "/snapshots/test:tag"
	err := (remoteRegistryImageDeleter{}).Delete(context.Background(), imageReference, recordedDigest, nil, true)
	require.NoError(t, err)
	select {
	case path := <-deletedPaths:
		assert.Equal(t, "/v2/snapshots/test/manifests/"+recordedDigest, path)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recorded-digest DELETE request")
	}
	select {
	case path := <-deletedPaths:
		assert.Equal(t, "/v2/snapshots/test/manifests/"+registryDigest, path)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fallback registry DELETE request")
	}
}

func TestRegistryAuthenticator_ReadsDockerConfigJSON(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-secret", Namespace: "default"},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://registry.example.com/v1/":{"username":"user","password":"pass"}}}`),
		},
	}

	authenticator, err := registryAuthenticator(secret, "registry.example.com")
	require.NoError(t, err)
	config, err := authn.Authorization(context.Background(), authenticator)
	require.NoError(t, err)
	assert.Equal(t, "user", config.Username)
	assert.Equal(t, "pass", config.Password)
}

func TestRegistryAuthenticator_RejectsSecretWithoutMatchingRegistry(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-secret", Namespace: "default"},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"other.example.com":{"auth":"dXNlcjpwYXNz"}}}`),
		},
	}

	_, err := registryAuthenticator(secret, "registry.example.com")
	require.ErrorContains(t, err, "has no credentials for registry.example.com")
}

func TestRegistryAuthenticator_AllowsAnonymousRegistry(t *testing.T) {
	authenticator, err := registryAuthenticator(nil, "registry.example.com")
	require.NoError(t, err)
	assert.Equal(t, authn.Anonymous, authenticator)
}

func TestRegistryAuthenticator_RejectsCredentialHelpers(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-secret", Namespace: "default"},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"credHelpers":{"registry.example.com":"osxkeychain"}}`),
		},
	}

	_, err := registryAuthenticator(secret, "registry.example.com")
	require.ErrorContains(t, err, "requires inline auths credentials")
}
