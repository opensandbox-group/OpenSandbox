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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Test_PROJECT_File_Resource_Registration verifies that the PROJECT file
// is correctly synchronised with the actual API types defined in the codebase.
// See: https://github.com/alibaba/OpenSandbox/issues/1079
func Test_PROJECT_File_Resource_Registration(t *testing.T) {
	projectPath := filepath.Join("..", "..", "PROJECT")
	data, err := os.ReadFile(projectPath)
	require.NoError(t, err, "PROJECT file must be readable")

	var project ProjectConfig
	err = yaml.Unmarshal(data, &project)
	require.NoError(t, err, "PROJECT file must be valid YAML")

	kinds := make(map[string]bool)
	for _, res := range project.Resources {
		kinds[res.Kind] = true
	}

	// Anomaly 1: Sandbox should NOT be registered (no types/controller files exist).
	require.False(t, kinds["Sandbox"],
		"PROJECT must not contain stale 'Sandbox' entry — no corresponding types or controller files exist")

	// Anomaly 2: SandboxSnapshot SHOULD be registered (types and controller exist).
	require.True(t, kinds["SandboxSnapshot"],
		"PROJECT must contain 'SandboxSnapshot' entry — types and controller files exist")

	// Sanity: existing types that were already correct.
	require.True(t, kinds["BatchSandbox"], "PROJECT must contain 'BatchSandbox'")
	require.True(t, kinds["Pool"], "PROJECT must contain 'Pool'")
}

// Test_PROJECT_Resource_Paths_Are_Importable verifies every registered resource
// points at a real Go API package.
func Test_PROJECT_Resource_Paths_Are_Importable(t *testing.T) {
	projectPath := filepath.Join("..", "..", "PROJECT")
	data, err := os.ReadFile(projectPath)
	require.NoError(t, err, "PROJECT file must be readable")

	var project ProjectConfig
	err = yaml.Unmarshal(data, &project)
	require.NoError(t, err, "PROJECT file must be valid YAML")

	for _, res := range project.Resources {
		require.NotEmpty(t, res.Path, "PROJECT resource %s must define path", res.Kind)

		cmd := exec.Command("go", "list", res.Path)
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "PROJECT resource %s path %q must resolve with go list: %s", res.Kind, res.Path, string(out))
		require.Equal(t, res.Path, strings.TrimSpace(string(out)))
	}
}

// Test_SandboxSnapshot_RBAC_Roles_Exist verifies the admin/editor/viewer
// ClusterRole manifests are present for SandboxSnapshot, matching the pattern
// used by BatchSandbox and Pool.
func Test_SandboxSnapshot_RBAC_Roles_Exist(t *testing.T) {
	rbacDir := filepath.Join("..", "..", "config", "rbac")
	roles := []string{
		"sandboxsnapshot_admin_role.yaml",
		"sandboxsnapshot_editor_role.yaml",
		"sandboxsnapshot_viewer_role.yaml",
	}
	for _, f := range roles {
		path := filepath.Join(rbacDir, f)
		data, err := os.ReadFile(path)
		require.NoError(t, err, "RBAC role file must exist: %s", f)

		var role ClusterRole
		err = yaml.Unmarshal(data, &role)
		require.NoError(t, err, "RBAC role file must be valid YAML: %s", f)
		require.NotEmpty(t, role.Rules, "RBAC role must have rules: %s", f)
	}
}

// Test_SandboxSnapshot_RBAC_Kustomization verifies the RBAC kustomization
// includes the SandboxSnapshot admin/editor/viewer role files.
func Test_SandboxSnapshot_RBAC_Kustomization(t *testing.T) {
	kustPath := filepath.Join("..", "..", "config", "rbac", "kustomization.yaml")
	data, err := os.ReadFile(kustPath)
	require.NoError(t, err, "kustomization.yaml must be readable")

	var kust RBACKustomization
	err = yaml.Unmarshal(data, &kust)
	require.NoError(t, err, "kustomization.yaml must be valid YAML")

	required := []string{
		"sandboxsnapshot_admin_role.yaml",
		"sandboxsnapshot_editor_role.yaml",
		"sandboxsnapshot_viewer_role.yaml",
	}
	for _, r := range required {
		require.Contains(t, kust.Resources, r,
			"RBAC kustomization must include %s", r)
	}
}

// Test_Helm_Controller_ClusterRole_Covers_Generated_Role verifies that the
// manager ClusterRole shipped by the controller Helm chart grants every
// permission controller-gen derives from the +kubebuilder:rbac markers, so a
// Helm-installed controller is not denied an API call the kustomize install
// allows.
func Test_Helm_Controller_ClusterRole_Covers_Generated_Role(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err, "generated role.yaml must be readable")
	var generated ClusterRole
	require.NoError(t, yaml.Unmarshal(data, &generated), "generated role.yaml must be valid YAML")
	require.NotEmpty(t, generated.Rules)

	chartPath := filepath.Join("..", "..", "..", "manifests", "charts", "controller", "templates", "clusterrole.yaml")
	data, err = os.ReadFile(chartPath)
	require.NoError(t, err, "controller chart clusterrole.yaml must be readable")

	// The manager ClusterRole document is plain YAML apart from template
	// directives on metadata lines; drop those lines and parse the rest.
	granted := map[string]bool{}
	found := false
	for _, doc := range strings.Split(string(data), "\n---\n") {
		if !strings.Contains(doc, `include "opensandbox.managerRoleName"`) {
			continue
		}
		var kept []string
		for _, line := range strings.Split(doc, "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		var role ClusterRole
		require.NoError(t, yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &role))
		for _, rule := range role.Rules {
			for _, g := range rule.APIGroups {
				for _, r := range rule.Resources {
					for _, v := range rule.Verbs {
						granted[g+"|"+r+"|"+v] = true
					}
				}
			}
		}
		found = true
	}
	require.True(t, found, "controller chart must define the manager ClusterRole")

	for _, rule := range generated.Rules {
		for _, g := range rule.APIGroups {
			for _, r := range rule.Resources {
				for _, v := range rule.Verbs {
					require.True(t, granted[g+"|"+r+"|"+v],
						"controller chart manager ClusterRole is missing apiGroup=%q resource=%q verb=%q from config/rbac/role.yaml", g, r, v)
				}
			}
		}
	}
}

// --- minimal YAML structs for test-only parsing ---

type ProjectConfig struct {
	Resources []ResourceEntry `yaml:"resources"`
}

type ResourceEntry struct {
	API struct {
		CRDVersion string `yaml:"crdVersion"`
	} `yaml:"api"`
	Controller bool   `yaml:"controller"`
	Domain     string `yaml:"domain"`
	Group      string `yaml:"group"`
	Kind       string `yaml:"kind"`
	Path       string `yaml:"path"`
	Version    string `yaml:"version"`
}

type ClusterRole struct {
	Kind  string `yaml:"kind"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

type RBACKustomization struct {
	Resources []string `yaml:"resources"`
}
