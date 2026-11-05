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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestShardTaskPatchAdmissionValidation(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	config, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })
	client, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	resource := client.Resource(schema.GroupVersionResource{
		Group: "sandbox.opensandbox.io", Version: "v1alpha1", Resource: "batchsandboxes",
	}).Namespace("default")
	for i, tt := range []struct {
		args        string
		wantInvalid bool
	}{
		{args: "3600", wantInvalid: true},
		{args: `"3600"`},
	} {
		object := &unstructured.Unstructured{}
		manifest := fmt.Sprintf(`{"apiVersion":"sandbox.opensandbox.io/v1alpha1","kind":"BatchSandbox","metadata":{"name":%q,"namespace":"default"},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"main","image":"busybox"}]}},"taskTemplate":{"spec":{"process":{"command":["sleep"]}}},"shardTaskPatches":[{"spec":{"process":{"args":[%s]}}}]}}`, fmt.Sprintf("shard-patch-%d", i), tt.args)
		require.NoError(t, json.Unmarshal([]byte(manifest), &object.Object))
		_, err := resource.Create(context.Background(), object, metav1.CreateOptions{})
		if tt.wantInvalid {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}
