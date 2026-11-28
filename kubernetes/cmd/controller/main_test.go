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

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "run-controller" {
		os.Args = os.Args[1:]
		main()
		return
	}
	os.Exit(m.Run())
}

func TestControllerInformerSelectors(t *testing.T) {
	const (
		podSelector = "batch-sandbox.sandbox.opensandbox.io/name"
		jobSelector = "sandbox.opensandbox.io/sandbox-snapshot-name"
	)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	config, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	target, err := url.Parse(config.Host)
	require.NoError(t, err)
	transport, err := rest.TransportFor(config)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	for _, tc := range []struct {
		name, namespace, pod, job string
	}{
		{name: "defaults"},
		{name: "namespace_only", namespace: "tenant-a"},
		{name: "pod_only", pod: "app in (a,b)," + podSelector},
		{name: "job_only", job: jobSelector},
		{name: "labels_and_namespace", namespace: "tenant-a", pod: podSelector, job: jobSelector},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resourcePrefix := ""
			args := []string{"run-controller", "--health-probe-bind-address=0"}
			if tc.namespace != "" {
				resourcePrefix = "/namespaces/" + tc.namespace
				args = append(args, "--namespaces="+tc.namespace)
			}
			if tc.pod != "" {
				args = append(args, "--pod-label-selector="+tc.pod)
			}
			if tc.job != "" {
				args = append(args, "--job-label-selector="+tc.job)
			}

			selectors := map[string]string{
				"/api/v1" + resourcePrefix + "/pods":                                           tc.pod,
				"/apis/batch/v1" + resourcePrefix + "/jobs":                                    tc.job,
				"/apis/sandbox.opensandbox.io/v1alpha1" + resourcePrefix + "/batchsandboxes":   "",
				"/apis/sandbox.opensandbox.io/v1alpha1" + resourcePrefix + "/pools":            "",
				"/apis/sandbox.opensandbox.io/v1alpha1" + resourcePrefix + "/sandboxsnapshots": "",
			}
			type informerRequest struct {
				path, selector string
				watch          bool
			}
			requests := make(chan informerRequest, 64)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, tracked := selectors[r.URL.Path]; tracked && r.Method == http.MethodGet {
					query := r.URL.Query()
					select {
					case requests <- informerRequest{r.URL.Path, query.Get("labelSelector"), query.Get("watch") == "true"}:
					case <-r.Context().Done():
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()

			kubeconfig := clientcmdapi.Config{
				Clusters:       map[string]*clientcmdapi.Cluster{"test": {Server: server.URL}},
				Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test"}},
				CurrentContext: "test",
			}
			path := filepath.Join(t.TempDir(), "kubeconfig")
			require.NoError(t, clientcmd.WriteToFile(kubeconfig, path))

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+path, "POD_NAMESPACE=")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			defer func() {
				cancel()
				<-exited
			}()

			seen := make(map[informerRequest]bool)
			for len(seen) < 2*len(selectors) {
				select {
				case request := <-requests:
					require.Equal(t, selectors[request.path], request.selector, "labelSelector for %s (watch=%v)", request.path, request.watch)
					seen[request] = true
				case err := <-exited:
					exited <- err
					firstLine, _, _ := strings.Cut(output.String(), "\n")
					t.Fatalf("controller exited before starting scoped informers: %v: %s", err, firstLine)
				case <-ctx.Done():
					t.Fatalf("waiting for lists and watches for %v, received %v", selectors, seen)
				}
			}
		})
	}
}

func TestControllerRejectsInvalidInformerSelectors(t *testing.T) {
	for _, flag := range []string{"pod-label-selector", "job-label-selector"} {
		t.Run(flag, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "run-controller", "--"+flag+"=app in (")
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			require.Equal(t, 1, exitErr.ExitCode(), "%s", output)
			require.Contains(t, string(output), "invalid --"+flag)
		})
	}
}
