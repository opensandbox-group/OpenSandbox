// Copyright 2026 The OpenSandbox Authors
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

package opensandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestFilesWithIdentityPreservesTransportAndDefaultClient(t *testing.T) {
	paths := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(execdAuthHeader) != "secret" || r.Header.Get("X-Routing") != "sandbox" {
			t.Error("identity request lost authentication or routing headers")
		}
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	const prefix = "/sandboxes/example/port/44772"
	original := NewExecdClient(server.URL+prefix, "secret", WithHTTPClient(server.Client()))
	original.client.headers = map[string]string{"X-Routing": "sandbox"}
	sandbox := &Sandbox{execd: original}
	a, err := sandbox.FilesWithIdentity(1001, 2000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sandbox.FilesWithIdentity(1002, 2000)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, client := range []IdentityFilesystem{a, b} {
		wg.Add(1)
		go func(client IdentityFilesystem) {
			defer wg.Done()
			if _, err := client.GetFileInfo(context.Background(), "/file"); err != nil {
				t.Error(err)
			}
		}(client)
	}
	wg.Wait()
	got := map[string]bool{<-paths: true, <-paths: true}
	for _, path := range []string{
		prefix + "/v1/filesystem/1001/2000/files/info",
		prefix + "/v1/filesystem/1002/2000/files/info",
	} {
		if !got[path] {
			t.Errorf("missing request for %s", path)
		}
	}
	body, err := a.DownloadFile(context.Background(), "/file", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if path := <-paths; path != prefix+"/v1/filesystem/1001/2000/files/download" {
		t.Errorf("download path = %s", path)
	}
	if _, err := original.GetFileInfo(context.Background(), "/original"); err != nil {
		t.Fatal(err)
	}
	if path := <-paths; path != prefix+"/files/info" {
		t.Errorf("original client changed: %s", path)
	}
}

func TestFilesWithIdentityNeverFallsBack(t *testing.T) {
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "unsupported route")
	}))
	defer server.Close()
	client := NewExecdClient(server.URL, "")
	files, err := client.FilesWithIdentity(1001, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.DeleteFiles(context.Background(), []string{"/file"}); err == nil {
		t.Fatal("unsupported identity route succeeded")
	}
	if len(requests) != 1 {
		t.Fatalf("sent %d requests, want one", len(requests))
	}
	if path := <-requests; path != "/v1/filesystem/1001/2000/files" {
		t.Errorf("unexpected path %s", path)
	}
}

func TestFilesWithIdentityValidation(t *testing.T) {
	client := NewExecdClient("http://localhost:44772", "")
	for _, pair := range [][2]uint32{{^uint32(0), 0}, {0, ^uint32(0)}} {
		if _, err := client.FilesWithIdentity(pair[0], pair[1]); err == nil {
			t.Fatal("accepted reserved ID")
		}
	}
	if _, err := (&Sandbox{}).FilesWithIdentity(1001, 2000); err == nil {
		t.Fatal("accepted uninitialized client")
	}
}
