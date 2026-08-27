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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadImageCommitterPodTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod-template.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
metadata:
  labels:
    identity.example/use: "true"
spec:
  serviceAccountName: snapshot-committer
  containers:
    - name: commit
      resources:
        requests:
          cpu: 100m
`), 0o600))

	template, err := loadImageCommitterPodTemplate(path)
	require.NoError(t, err)
	assert.Equal(t, "true", template.Labels["identity.example/use"])
	assert.Equal(t, "snapshot-committer", template.Spec.ServiceAccountName)
	require.Len(t, template.Spec.Containers, 1)
	assert.Equal(t, "100m", template.Spec.Containers[0].Resources.Requests.Cpu().String())

	template, err = loadImageCommitterPodTemplate("")
	require.NoError(t, err)
	assert.Nil(t, template)
}

func TestLoadImageCommitterPodTemplateRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod-template.yaml")
	require.NoError(t, os.WriteFile(path, []byte("spec:\n  unknownField: true\n"), 0o600))
	_, err := loadImageCommitterPodTemplate(path)
	require.Error(t, err)
}
