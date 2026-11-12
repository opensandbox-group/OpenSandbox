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

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/stretchr/testify/require"
)

func TestRevisionRuntimeStartupConfiguration(t *testing.T) {
	clearFSBInterceptionEnv(t)
	for _, item := range []struct {
		name, profile, live, extras string
		wantError                   bool
	}{
		{"live-extra", "", "true", "8443", true},
		{"live-extras", "", "true", "8080,8443", true},
		{"live-unset", "", "true", "", false},
		{"live-empty", "", "true", "", false},
		{"live-whitespace", "", "true", "  ", false},
		{"live-invalid", "", "true", "not-a-port", true},
		{"live-reserved", "", "true", "443", true},
		{"nonlive-extra", "", "", "8443", false},
		{"nonlive-false-extra", "", "false", "8443", false},
		{"nonlive-invalid-still-validated-by-launcher", "", "", "bad", false},
		{"fast-live", constants.ProfileFastSandbox, "true", "", false},
		{"fast-live-extra", constants.ProfileFastSandbox, "true", "8443", false},
		{"fast-nonlive-extra", constants.ProfileFastSandbox, "", "8443", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv(constants.EnvExperimentalRevisionRuntime, item.live)
			t.Setenv(constants.EnvMitmproxyExtraPorts, item.extras)
			if item.name == "live-unset" {
				require.NoError(t, os.Unsetenv(constants.EnvMitmproxyExtraPorts))
			}
			err := validateRevisionRuntimeStartup(item.profile)
			if item.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if item.extras == "8443" {
					// The legacy/all interception list still includes extra ports.
					ports, err := constants.BuildMitmproxyPortList(item.extras)
					require.NoError(t, err)
					require.Equal(t, "80,443,8443", ports)
				}
			}
		})
	}
}

func TestRevisionRuntimeStartupRejectsBeforeProfileOrNetwork(t *testing.T) {
	const child = "OPENSANDBOX_TEST_REVISION_STARTUP_CHILD"
	if os.Getenv(child) == "1" {
		main()
		return
	}
	clearFSBInterceptionEnv(t)
	for _, item := range []struct{ name, profile, extras, diagnostic string }{
		{"sidecar-extra", "", "8443", constants.EnvMitmproxyExtraPorts},
		{"fast-live", constants.ProfileFastSandbox, "", "fast-sandbox Actions protocol:"},
	} {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv(child, "1")
			t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
			t.Setenv(constants.EnvEgressProfile, item.profile)
			t.Setenv(constants.EnvMitmproxyExtraPorts, item.extras)
			if item.profile == constants.ProfileFastSandbox {
				// Reach profile dispatch, but stop before any network setup.
				t.Setenv(constants.EnvFastSandboxActionsAPIVersion, "invalid")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRevisionRuntimeStartupRejectsBeforeProfileOrNetwork$")
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.NoError(t, ctx.Err())
			if item.profile == constants.ProfileFastSandbox {
				require.NotContains(t, string(output), "invalid live revision runtime configuration")
			} else {
				require.Contains(t, string(output), "invalid live revision runtime configuration")
			}
			require.Contains(t, string(output), item.diagnostic)
		})
	}
}

// The public profile switch must reject unsupported settings before dispatch.
func TestFSBInterceptionStartupRejections(t *testing.T) {
	const modeEnv = "OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE"
	for _, tc := range []struct{ name, profile, mode, revision, admission, key, value, diagnostic string }{
		{"sidecar-all", "sidecar", "all", "", "", "", "", modeEnv},
		{"sidecar-bound", "", "credential-bound", "", "", "", "", modeEnv},
		{"invalid-mode", "fast-sandbox", "invalid-private-value", "", "", "", "", modeEnv},
		{"explicit-empty", "fast-sandbox", "", "", "", "", "", modeEnv},
		{"all-revision", "fast-sandbox", "all", "true", "", "", "", constants.EnvExperimentalRevisionRuntime},
		{"all-admission", "fast-sandbox", "all", "", "true", "", "", constants.EnvExperimentalFSBTLSAdmission},
		{"bound-revision-false", "fast-sandbox", "credential-bound", "false", "", "", "", constants.EnvExperimentalRevisionRuntime},
		{"bound-admission-false", "fast-sandbox", "credential-bound", "", "false", "", "", constants.EnvExperimentalFSBTLSAdmission},
		{"extra-ports", "fast-sandbox", "credential-bound", "", "", constants.EnvMitmproxyExtraPorts, "8443", constants.EnvMitmproxyExtraPorts},
		{"invalid-ports", "fast-sandbox", "credential-bound", "", "", constants.EnvMitmproxyExtraPorts, "private-value", constants.EnvMitmproxyExtraPorts},
		{"insecure", "fast-sandbox", "credential-bound", "", "", constants.EnvMitmproxySslInsecure, "true", constants.EnvMitmproxySslInsecure},
		{"proxy", "fast-sandbox", "credential-bound", "", "", constants.EnvUpstreamProxy, "https://private.invalid:443", constants.EnvUpstreamProxy},
		{"empty-transparent", "fast-sandbox", "credential-bound", "", "", constants.EnvMitmproxyTransparent, "", constants.EnvMitmproxyTransparent},
		{"empty-policy-mode", "fast-sandbox", "credential-bound", "", "", constants.EnvEgressMode, "", constants.EnvEgressMode},
		{"no-transparent", "fast-sandbox", "credential-bound", "", "", constants.EnvMitmproxyTransparent, "false", constants.EnvMitmproxyTransparent},
		{"dns-only", "fast-sandbox", "credential-bound", "", "", constants.EnvEgressMode, "dns", constants.EnvEgressMode},
		{"invalid-policy-mode", "fast-sandbox", "credential-bound", "", "", constants.EnvEgressMode, "private-value", constants.EnvEgressMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(modeEnv, tc.mode)
			t.Setenv(constants.EnvExperimentalRevisionRuntime, tc.revision)
			t.Setenv(constants.EnvExperimentalFSBTLSAdmission, tc.admission)
			if tc.revision == "" {
				require.NoError(t, os.Unsetenv(constants.EnvExperimentalRevisionRuntime))
			}
			if tc.admission == "" {
				require.NoError(t, os.Unsetenv(constants.EnvExperimentalFSBTLSAdmission))
			}
			t.Setenv(constants.EnvMitmproxyTransparent, "true")
			t.Setenv(constants.EnvEgressMode, "dns+nft")
			t.Setenv(constants.EnvMitmproxyExtraPorts, "")
			t.Setenv(constants.EnvMitmproxySslInsecure, "")
			t.Setenv(constants.EnvUpstreamProxy, "")
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}
			for _, legacy := range []bool{false, true} {
				if legacy {
					if tc.mode != "credential-bound" || tc.key == "" {
						continue
					}
					require.NoError(t, os.Unsetenv(modeEnv))
					t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
					t.Setenv(constants.EnvExperimentalFSBTLSAdmission, "true")
				}
				err := validateRevisionRuntimeStartup(tc.profile)
				require.ErrorContains(t, err, tc.diagnostic)
				require.NotContains(t, err.Error(), "private")
			}
		})
	}
}

func clearFSBInterceptionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{constants.EnvFSBInterceptionMode, constants.EnvExperimentalRevisionRuntime, constants.EnvExperimentalFSBTLSAdmission} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	for _, name := range []string{constants.EnvMitmproxyExtraPorts, constants.EnvMitmproxySslInsecure, constants.EnvUpstreamProxy} {
		t.Setenv(name, "")
	}
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, "dns+nft")
}

func TestFSBInterceptionModeCombinations(t *testing.T) {
	// Every combination of absent/false/true legacy gates and absent/all/bound
	// public mode, including both default and explicit sidecar profiles.
	for _, profile := range []string{"", constants.ProfileSidecar, constants.ProfileFastSandbox} {
		for _, public := range []string{"absent", "all", "credential-bound"} {
			for _, revision := range []string{"absent", "false", "true"} {
				for _, admission := range []string{"absent", "false", "true"} {
					t.Run(strings.Join([]string{profile, public, revision, admission}, "/"), func(t *testing.T) {
						clearFSBInterceptionEnv(t)
						for key, value := range map[string]string{constants.EnvFSBInterceptionMode: public, constants.EnvExperimentalRevisionRuntime: revision, constants.EnvExperimentalFSBTLSAdmission: admission} {
							if value != "absent" {
								t.Setenv(key, value)
							}
						}
						mode, err := resolveFastSandboxInterceptionMode(profile)
						invalid := public != "absent" && profile != constants.ProfileFastSandbox
						invalid = invalid || public == "all" && (revision == "true" || admission == "true")
						invalid = invalid || public == "credential-bound" && (revision == "false" || admission == "false")
						invalid = invalid || public == "absent" && admission == "true" && (revision != "true" || profile != constants.ProfileFastSandbox)
						if invalid {
							require.Error(t, err)
							return
						}
						require.NoError(t, err)
						install := profile == constants.ProfileFastSandbox && (public == "credential-bound" || public == "absent" && revision == "true")
						bound := profile == constants.ProfileFastSandbox && (public == "credential-bound" || public == "absent" && admission == "true")
						require.Equal(t, fastSandboxInterceptionMode{installReceiver: install, credentialBound: bound, deprecatedAlias: public == "absent" && bound}, mode)
					})
				}
			}
		}
	}
}

func TestFSBInterceptionMitmproxyOptions(t *testing.T) {
	for _, tc := range []struct{ name, yaml, yml, diagnostic string }{
		{"missing", "", "", ""},
		{"empty-lists", "ignore_hosts: []\nallow_hosts: []\nssl_insecure: false\n", "", ""},
		{"ignore", "ignore_hosts: ['private.invalid']", "", "ignore_hosts"},
		{"allow", "allow_hosts: ['private.invalid']", "", "allow_hosts"},
		{"yml-only", "", "allow_hosts: ['private.invalid']", "allow_hosts"},
		{"yml-overrides", "ignore_hosts: ['private.invalid']", "ignore_hosts: []", ""},
		{"yml-adds", "ignore_hosts: []", "ignore_hosts: ['private.invalid']", "ignore_hosts"},
		{"alias", "hosts: &hosts ['private.invalid']\nignore_hosts: *hosts", "", "ignore_hosts"},
		{"merge", "base: &base {allow_hosts: ['private.invalid']}\n<<: *base", "", "allow_hosts"},
		{"scalar-list", "ignore_hosts: private.invalid", "", "ignore_hosts"},
		{"null-list", "allow_hosts: null", "", "allow_hosts"},
		{"bad-list-item", "ignore_hosts: [123]", "", "ignore_hosts"},
		{"malformed", "private: [", "", "invalid mitmproxy configuration"},
		{"duplicate", "ignore_hosts: []\nignore_hosts: ['private.invalid']", "", "invalid mitmproxy configuration"},
		{"multiple-docs", "ignore_hosts: []\n---\nallow_hosts: []", "", "invalid mitmproxy configuration"},
		{"root-list", "- private.invalid", "", "invalid mitmproxy configuration"},
		{"static-insecure", "ssl_insecure: true", "", "ssl_insecure"},
		{"invalid-static-insecure", "ssl_insecure: private", "", "ssl_insecure"},
		{"yml-secure-overrides", "ssl_insecure: true", "ssl_insecure: false", ""},
		{"unreadable", "directory", "", "cannot read mitmproxy configuration"},
	} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%v", tc.name, legacy), func(t *testing.T) {
				clearFSBInterceptionEnv(t)
				if legacy {
					t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
					t.Setenv(constants.EnvExperimentalFSBTLSAdmission, "true")
				} else {
					t.Setenv(constants.EnvFSBInterceptionMode, "credential-bound")
				}
				home := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(home, ".mitmproxy"), 0700))
				for name, content := range map[string]string{"config.yaml": tc.yaml, "config.yml": tc.yml} {
					path := filepath.Join(home, ".mitmproxy", name)
					if content == "directory" {
						require.NoError(t, os.Mkdir(path, 0700))
					} else if content != "" {
						require.NoError(t, os.WriteFile(path, []byte(content), 0600))
					}
				}
				old := fsbMitmproxyHome
				fsbMitmproxyHome = func() (string, error) { return home, nil }
				t.Cleanup(func() { fsbMitmproxyHome = old })
				mode, err := revisionRuntimeStartup(constants.ProfileFastSandbox)
				if tc.diagnostic == "" {
					require.NoError(t, err)
					require.True(t, mode.credentialBound)
				} else {
					require.ErrorContains(t, err, tc.diagnostic)
					require.NotContains(t, err.Error(), "private")
				}
			})
		}
	}
}

func TestFSBInterceptionDisabledStartupUnchanged(t *testing.T) {
	clearFSBInterceptionEnv(t)
	oldLookup, oldMode := fsbMitmproxyHome, startupFSBInterceptionMode
	fsbMitmproxyHome = func() (string, error) {
		t.Fatal("disabled switch must not read mitmproxy configuration")
		return "", nil
	}
	t.Cleanup(func() { fsbMitmproxyHome, startupFSBInterceptionMode = oldLookup, oldMode })
	before := os.Environ()
	mode, err := revisionRuntimeStartup(constants.ProfileFastSandbox)
	require.NoError(t, err)
	require.Equal(t, fastSandboxInterceptionMode{}, mode)
	startupFSBInterceptionMode = &mode
	owner, err := newFastSandboxSubjectLaunchOwner(false, -1, -1)
	require.NoError(t, err)
	require.Nil(t, owner)
	require.Equal(t, before, os.Environ(), "startup must not rewrite child environment")
}

func TestFSBInterceptionStartupRejectsBeforeDispatch(t *testing.T) {
	const child = "OPENSANDBOX_TEST_FSB_MODE_STARTUP_CHILD"
	if os.Getenv(child) == "1" {
		fsbMitmproxyHome = func() (string, error) { return os.Getenv("OPENSANDBOX_TEST_FSB_HOME"), nil }
		main()
		return
	}
	for _, tc := range []struct{ name, key, value, config, diagnostic string }{
		{"ports", constants.EnvMitmproxyExtraPorts, "8443", "", constants.EnvMitmproxyExtraPorts},
		{"insecure", constants.EnvMitmproxySslInsecure, "true", "", constants.EnvMitmproxySslInsecure},
		{"proxy", constants.EnvUpstreamProxy, "https://private.invalid:443", "", constants.EnvUpstreamProxy},
		{"transparent", constants.EnvMitmproxyTransparent, "false", "", constants.EnvMitmproxyTransparent},
		{"policy-mode", constants.EnvEgressMode, "dns", "", constants.EnvEgressMode},
		{"ignore", "", "", "ignore_hosts: ['private.invalid']", "ignore_hosts"},
		{"allow", "", "", "allow_hosts: ['private.invalid']", "allow_hosts"},
		{"alias", constants.EnvFSBInterceptionMode, "all", "", constants.EnvExperimentalRevisionRuntime},
		{"deprecated", "", "", "", "fast-sandbox Actions protocol:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearFSBInterceptionEnv(t)
			t.Setenv(child, "1")
			t.Setenv(constants.EnvEgressProfile, constants.ProfileFastSandbox)
			t.Setenv(constants.EnvFSBInterceptionMode, "credential-bound")
			t.Setenv(constants.EnvFastSandboxActionsAPIVersion, "invalid")
			home := t.TempDir()
			t.Setenv("OPENSANDBOX_TEST_FSB_HOME", home)
			require.NoError(t, os.Mkdir(filepath.Join(home, ".mitmproxy"), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".mitmproxy", "config.yaml"), []byte(tc.config), 0600))
			if tc.name == "deprecated" {
				require.NoError(t, os.Unsetenv(constants.EnvFSBInterceptionMode))
				t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
				t.Setenv(constants.EnvExperimentalFSBTLSAdmission, "true")
			}
			if tc.name == "alias" {
				t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
			}
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFSBInterceptionStartupRejectsBeforeDispatch$").CombinedOutput()
			require.Error(t, err)
			require.NoError(t, ctx.Err())
			require.Contains(t, string(output), tc.diagnostic)
			require.NotContains(t, string(output), "private")
			if tc.name == "deprecated" {
				require.Equal(t, 1, strings.Count(string(output), "are deprecated"))
			} else {
				require.NotContains(t, string(output), "fast-sandbox Actions protocol:")
			}
		})
	}
}

func TestFSBInterceptionConfigurationLookupFailure(t *testing.T) {
	clearFSBInterceptionEnv(t)
	t.Setenv(constants.EnvFSBInterceptionMode, "credential-bound")
	old := fsbMitmproxyHome
	fsbMitmproxyHome = func() (string, error) { return "", fmt.Errorf("private hostname and uid") }
	t.Cleanup(func() { fsbMitmproxyHome = old })
	err := validateRevisionRuntimeStartup(constants.ProfileFastSandbox)
	require.EqualError(t, err, "Fast Sandbox credential-bound: mitmproxy configuration unavailable")
}
