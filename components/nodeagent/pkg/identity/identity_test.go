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

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOSSAndFinalizeIdentityVectors(t *testing.T) {
	target, err := OSSTargetID("HTTPS://OSS-CN.EXAMPLE.COM:443/", "bucket-a", "/logs/", "prod-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:400cd6f60156b9e0c8165c3c228764974b80e0e84003abece9040c5ec8b28ec6"; target != want {
		t.Fatalf("target=%q want=%q", target, want)
	}
	if got, want := FinalizeID("container-logs/u123/sandbox", 2, "sha256:target"), "sha256:499a3cc01ca0b84d76764b8d7c60ec990fc42267278107ee9dc93a8052d58c96"; got != want {
		t.Fatalf("finalize=%q want=%q", got, want)
	}
}

func TestFileTargetIDStableBeforeAndAfterRootCreation(t *testing.T) {
	realRoot := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(realRoot, linkParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(linkParent, "logs", "nodeagent")
	before, err := FileTargetID(root, "cluster", "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := FileTargetID(root, "cluster", "node")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("target changed after root creation: before=%s after=%s", before, after)
	}
}

func TestOSSTargetRejectsNonOrigin(t *testing.T) {
	for _, endpoint := range []string{"http://oss.example.com", "https://oss.example.com/path", "https://user@oss.example.com"} {
		if _, err := OSSTargetID(endpoint, "bucket", "logs", "prod"); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}
}

func TestCanonicalOSSEndpoint(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: "HTTPS://OSS.Example.COM:443/", want: "https://oss.example.com"},
		{raw: "https://[2001:DB8::1]:443/", want: "https://[2001:db8::1]"},
		{raw: "https://OSS.Example.COM:8443/", want: "https://oss.example.com:8443"},
	} {
		got, err := CanonicalOSSEndpoint(test.raw)
		if err != nil || got != test.want {
			t.Fatalf("CanonicalOSSEndpoint(%q) = %q, %v; want %q", test.raw, got, err, test.want)
		}
	}
	for _, endpoint := range []string{
		"http://oss.example.com",
		"https://user:password@oss.example.com",
		"https://oss.example.com/path",
		"https://oss.example.com?query=value",
		"https://oss.example.com#fragment",
		"https://:443",
	} {
		if _, err := CanonicalOSSEndpoint(endpoint); err == nil {
			t.Fatalf("CanonicalOSSEndpoint(%q) unexpectedly succeeded", endpoint)
		}
	}
}
