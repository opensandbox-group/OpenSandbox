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

package policy

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadPolicyFromEnvVar(t *testing.T) {
	const envName = "TEST_EGRESS_POLICY"
	t.Setenv(envName, `{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"}]}`)

	pol, err := loadPolicyFromEnvVar(envName)
	require.NoError(t, err, "unexpected error")
	require.NotNil(t, pol, "expected parsed policy")
	require.Equal(t, ActionAllow, pol.Evaluate("example.com."), "expected parsed policy to allow example.com")

	t.Setenv(envName, "")
	pol, err = loadPolicyFromEnvVar(envName)
	require.NoError(t, err, "unexpected error on empty env")
	require.NotNil(t, pol, "expected default deny policy when env is empty")
	require.Equal(t, ActionDeny, pol.DefaultAction, "expected default deny when env is empty")
}

func TestLoadInitialPolicy_EmptyPathUsesEnv(t *testing.T) {
	const envName = "TEST_EGRESS_POLICY_LOAD1"
	t.Setenv(envName, `{"defaultAction":"deny","egress":[{"action":"allow","target":"a.example.com"}]}`)

	pol, err := LoadInitialPolicy("", envName)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, pol.Evaluate("a.example.com."))
}

func TestLoadInitialPolicy_MissingFileUsesEnv(t *testing.T) {
	const envName = "TEST_EGRESS_POLICY_LOAD2"
	t.Setenv(envName, `{"defaultAction":"deny","egress":[{"action":"allow","target":"b.example.com"}]}`)

	pol, err := LoadInitialPolicy(filepath.Join(t.TempDir(), "nonexistent-policy.json"), envName)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, pol.Evaluate("b.example.com."))
}

func TestLoadInitialPolicy_ValidFileOverridesEnv(t *testing.T) {
	const envName = "TEST_EGRESS_POLICY_LOAD3"
	t.Setenv(envName, `{"defaultAction":"deny","egress":[{"action":"allow","target":"from-env.example.com"}]}`)

	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	err := os.WriteFile(path, []byte(`{"defaultAction":"deny","egress":[{"action":"allow","target":"from-file.example.com"}]}`), 0o600)
	require.NoError(t, err)

	pol, err := LoadInitialPolicy(path, envName)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, pol.Evaluate("from-file.example.com."))
	require.Equal(t, ActionDeny, pol.Evaluate("from-env.example.com."))
}

func TestLoadInitialPolicy_InvalidFileFallsBackToEnv(t *testing.T) {
	const envName = "TEST_EGRESS_POLICY_LOAD4"
	t.Setenv(envName, `{"defaultAction":"deny","egress":[{"action":"allow","target":"fallback.example.com"}]}`)

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte(`not-json`), 0o600))

	pol, err := LoadInitialPolicy(path, envName)
	require.NoError(t, err)
	require.Equal(t, ActionAllow, pol.Evaluate("fallback.example.com."))
}

func TestSavePolicyFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	pol, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"x.example.com"}]}`)
	require.NoError(t, err)

	require.NoError(t, SavePolicyFile(path, pol))

	p2, err := LoadInitialPolicy(path, "TEST_EGRESS_UNUSED_ENV")
	require.NoError(t, err)
	require.Equal(t, ActionAllow, p2.Evaluate("x.example.com."))
}

func TestSavePolicyFile_NilWritesDefaultDeny(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nil.json")
	require.NoError(t, SavePolicyFile(path, nil))
	p2, err := LoadInitialPolicy(path, "TEST_EGRESS_UNUSED_ENV2")
	require.NoError(t, err)
	require.Equal(t, ActionDeny, p2.Evaluate("any.example.com."))
}

func TestSavePolicyFile_EmptyPathNoOp(t *testing.T) {
	require.NoError(t, SavePolicyFile("", DefaultDenyPolicy()))
}

func TestSavePolicyFile_PreservesMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o640))

	pol, err := ParsePolicy(`{"defaultAction":"deny","egress":[]}`)
	require.NoError(t, err)
	require.NoError(t, SavePolicyFile(path, pol))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o640), info.Mode()&fs.ModePerm)
}
