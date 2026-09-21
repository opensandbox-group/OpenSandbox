// Copyright 2025 The OpenSandbox Authors
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

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		expectedToken := "token test-token"
		if token != expectedToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	auth := NewAuth()
	auth.Token = "test-token"

	client := NewClient(&http.Client{}, auth)

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
}

func TestBasicAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "testuser" || password != "testpass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	auth := NewAuth()
	auth.Username = "testuser"
	auth.Password = "testpass"

	client := NewClient(&http.Client{}, auth)

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
}

func TestAuthValidation(t *testing.T) {
	emptyAuth := NewAuth()
	if emptyAuth.IsValid() {
		t.Error("Empty Auth should be invalid, but was determined to be valid")
	}

	tokenAuth := NewAuth()
	tokenAuth.Token = "test-token"
	if !tokenAuth.IsValid() {
		t.Error("Auth with token should be valid, but was determined to be invalid")
	}

	basicAuth := NewAuth()
	basicAuth.Username = "testuser"
	basicAuth.Password = "testpass"
	if !basicAuth.IsValid() {
		t.Error("Auth with Basic Auth should be valid, but was determined to be invalid")
	}

	invalidBasicAuth := NewAuth()
	invalidBasicAuth.Username = "testuser"
	if invalidBasicAuth.IsValid() {
		t.Error("Auth with only username and no password should be invalid, but was determined to be valid")
	}

	mixedAuth := NewAuth()
	mixedAuth.Token = "test-token"
	mixedAuth.Username = "testuser"
	mixedAuth.Password = "testpass"
	if !mixedAuth.IsValid() {
		t.Error("Auth with both token and Basic Auth should be valid, but was determined to be invalid")
	}
}
