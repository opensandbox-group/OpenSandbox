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

package flag

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPConnectionPoolFlags(t *testing.T) {
	originalFlags, originalArgs := flag.CommandLine, os.Args
	t.Cleanup(func() {
		flag.CommandLine, os.Args = originalFlags, originalArgs
	})
	for _, test := range []struct {
		name               string
		args               []string
		wantMaxIdleConns   int
		wantMaxIdlePerHost int
		wantIdleTimeout    time.Duration
		wantParseError     bool
	}{
		{name: "defaults", wantMaxIdleConns: 100, wantMaxIdlePerHost: 2, wantIdleTimeout: 90 * time.Second},
		{name: "custom", args: []string{"--http-max-idle-conns=1000", "--http-max-idle-conns-per-host=8", "--http-idle-conn-timeout=2m"}, wantMaxIdleConns: 1000, wantMaxIdlePerHost: 8, wantIdleTimeout: 2 * time.Minute},
		{name: "explicit zero", args: []string{"--http-max-idle-conns=0", "--http-max-idle-conns-per-host=0", "--http-idle-conn-timeout=0s"}},
		{name: "invalid duration", args: []string{"--http-idle-conn-timeout=invalid"}, wantParseError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			flag.CommandLine = flag.NewFlagSet("ingress", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = []string{"ingress"}
			InitFlags()
			err := flag.CommandLine.Parse(test.args)
			if (err != nil) != test.wantParseError {
				t.Fatalf("Parse() = %v, want error = %v", err, test.wantParseError)
			}
			if test.wantParseError {
				return
			}
			if HTTPMaxIdleConns != test.wantMaxIdleConns || HTTPMaxIdleConnsPerHost != test.wantMaxIdlePerHost || HTTPIdleConnTimeout != test.wantIdleTimeout {
				t.Fatalf("HTTP connection pool = (%d, %d, %s), want (%d, %d, %s)", HTTPMaxIdleConns, HTTPMaxIdleConnsPerHost, HTTPIdleConnTimeout, test.wantMaxIdleConns, test.wantMaxIdlePerHost, test.wantIdleTimeout)
			}
		})
	}
}

func TestFastSandboxNamespaceFlag(t *testing.T) {
	originalFlags, originalArgs := flag.CommandLine, os.Args
	t.Cleanup(func() {
		flag.CommandLine, os.Args = originalFlags, originalArgs
	})
	for _, test := range []struct {
		name  string
		args  []string
		want  string
		valid bool
	}{
		{name: "unset", valid: true},
		{name: "single namespace", args: []string{"--fast-sandbox-namespace", "tenant-a"}, want: "tenant-a", valid: true},
		{name: "single namespace with kubeconfig and auth", args: []string{"--fast-sandbox-namespace", "tenant-a", "--fastpath-auth-enabled=true", "--kubeconfig=/tmp/kc"}, want: "tenant-a", valid: true},
		{name: "maximum length", args: []string{"--fast-sandbox-namespace=" + strings.Repeat("a", 63)}, want: strings.Repeat("a", 63), valid: true},
		{name: "empty", args: []string{"--fast-sandbox-namespace="}},
		{name: "list", args: []string{"--fast-sandbox-namespace=tenant-a,tenant-b"}},
		{name: "space separated", args: []string{"--fast-sandbox-namespace=tenant-a tenant-b"}},
		{name: "wildcard", args: []string{"--fast-sandbox-namespace=*"}},
		{name: "uppercase", args: []string{"--fast-sandbox-namespace=Tenant"}},
		{name: "too long", args: []string{"--fast-sandbox-namespace=" + strings.Repeat("a", 64)}},
		{name: "multiple flags", args: []string{"--fast-sandbox-namespace=tenant-a", "--fast-sandbox-namespace=tenant-b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			flag.CommandLine = flag.NewFlagSet("ingress", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = []string{"ingress"}
			InitFlags()
			err := flag.CommandLine.Parse(test.args)
			if (err == nil) != test.valid {
				t.Fatalf("Parse() = %v, valid = %v", err, test.valid)
			}
			if test.valid && FastSandboxNamespace != test.want {
				t.Fatalf("namespace = %q, want %q", FastSandboxNamespace, test.want)
			}
		})
	}
}
