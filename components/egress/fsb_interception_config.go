// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"gopkg.in/yaml.v3"
)

type fastSandboxInterceptionMode struct {
	installReceiver bool
	credentialBound bool
	deprecatedAlias bool
}

// Published once by main, before profile dispatch. A nil value retains the
// existing constructor seam for callers that do not run process startup.
var startupFSBInterceptionMode *fastSandboxInterceptionMode

// This is the only parser for the Fast Sandbox switch and its legacy aliases.
// Never rewrite operator environment: in particular the disabled launch must
// preserve the original argv and child environment exactly.
func resolveFastSandboxInterceptionMode(profile string) (fastSandboxInterceptionMode, error) {
	var mode fastSandboxInterceptionMode
	raw, explicit := os.LookupEnv(constants.EnvFSBInterceptionMode)
	revision, revisionSet := os.LookupEnv(constants.EnvExperimentalRevisionRuntime)
	admission, admissionSet := os.LookupEnv(constants.EnvExperimentalFSBTLSAdmission)
	revisionOn, admissionOn := constants.IsTruthy(revision), constants.IsTruthy(admission)
	if explicit {
		if profile != constants.ProfileFastSandbox {
			return mode, fmt.Errorf("%s requires fast-sandbox profile", constants.EnvFSBInterceptionMode)
		}
		switch raw {
		case "all":
			for _, gate := range []struct {
				name    string
				enabled bool
			}{
				{constants.EnvExperimentalRevisionRuntime, revisionOn},
				{constants.EnvExperimentalFSBTLSAdmission, admissionOn},
			} {
				if gate.enabled {
					return mode, fmt.Errorf("%s=all conflicts with %s", constants.EnvFSBInterceptionMode, gate.name)
				}
			}
		case "credential-bound":
			for _, gate := range []struct {
				name             string
				present, enabled bool
			}{
				{constants.EnvExperimentalRevisionRuntime, revisionSet, revisionOn},
				{constants.EnvExperimentalFSBTLSAdmission, admissionSet, admissionOn},
			} {
				if gate.present && !gate.enabled {
					return mode, fmt.Errorf("%s=credential-bound conflicts with %s", constants.EnvFSBInterceptionMode, gate.name)
				}
			}
			mode.installReceiver, mode.credentialBound = true, true
		default:
			return mode, fmt.Errorf("%s must be all or credential-bound", constants.EnvFSBInterceptionMode)
		}
		return mode, nil
	}
	if admissionOn {
		if !revisionOn {
			return mode, fmt.Errorf("%s requires %s=true", constants.EnvExperimentalFSBTLSAdmission, constants.EnvExperimentalRevisionRuntime)
		}
		if profile != constants.ProfileFastSandbox {
			return mode, fmt.Errorf("%s requires fast-sandbox profile", constants.EnvExperimentalFSBTLSAdmission)
		}
	}
	if profile == constants.ProfileFastSandbox {
		mode.installReceiver, mode.credentialBound, mode.deprecatedAlias = revisionOn, admissionOn, revisionOn && admissionOn
	}
	return mode, nil
}

// Lookup uses the same runtime user and HOME as mitmproxy.Launch. Kept as a
// narrow test seam: startup validation never starts a proxy or touches networking.
var fsbMitmproxyHome = func() (string, error) {
	_, _, home, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	return home, err
}

func validateFastSandboxInterception(mode fastSandboxInterceptionMode) error {
	if !mode.credentialBound {
		return nil
	}
	ports, err := constants.BuildMitmproxyPortList(os.Getenv(constants.EnvMitmproxyExtraPorts))
	if err != nil || ports != constants.MitmproxyBasePorts {
		return fmt.Errorf("%s is not supported with Fast Sandbox credential-bound", constants.EnvMitmproxyExtraPorts)
	}
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxySslInsecure)) {
		return fmt.Errorf("%s is not supported with Fast Sandbox credential-bound", constants.EnvMitmproxySslInsecure)
	}
	if strings.TrimSpace(os.Getenv(constants.EnvUpstreamProxy)) != "" {
		return fmt.Errorf("%s is not supported with Fast Sandbox credential-bound", constants.EnvUpstreamProxy)
	}
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return fmt.Errorf("%s must be true for Fast Sandbox credential-bound", constants.EnvMitmproxyTransparent)
	}
	enforcement, err := constants.ParseEgressMode(os.Getenv(constants.EnvEgressMode))
	if err != nil || enforcement != constants.PolicyDnsNft {
		return fmt.Errorf("%s must be dns+nft for Fast Sandbox credential-bound", constants.EnvEgressMode)
	}
	home, err := fsbMitmproxyHome()
	if err != nil {
		return fmt.Errorf("Fast Sandbox credential-bound: mitmproxy configuration unavailable")
	}
	return validateFastSandboxMitmproxyOptions(home)
}

// mitmproxy 11.0.2 tools/main.py loads both files, in this order. Neither
// selectors nor their parse/read errors may escape into a diagnostic.
func validateFastSandboxMitmproxyOptions(home string) error {
	effective := make(map[string]any)
	for _, name := range []string{"config.yaml", "config.yml"} {
		content, err := os.ReadFile(filepath.Join(home, ".mitmproxy", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("Fast Sandbox credential-bound: cannot read mitmproxy configuration")
		}
		var options map[string]any
		decoder := yaml.NewDecoder(bytes.NewReader(content))
		if err := decoder.Decode(&options); err != nil && err != io.EOF {
			return fmt.Errorf("Fast Sandbox credential-bound: invalid mitmproxy configuration")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("Fast Sandbox credential-bound: invalid mitmproxy configuration")
		}
		for _, option := range []string{"ignore_hosts", "allow_hosts"} {
			if value, present := options[option]; present {
				list, ok := value.([]any)
				if !ok {
					return fmt.Errorf("Fast Sandbox credential-bound: invalid mitmproxy %s", option)
				}
				for _, entry := range list {
					if _, ok := entry.(string); !ok {
						return fmt.Errorf("Fast Sandbox credential-bound: invalid mitmproxy %s", option)
					}
				}
				effective[option] = list
			}
		}
		if value, present := options["ssl_insecure"]; present {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("Fast Sandbox credential-bound: invalid mitmproxy ssl_insecure")
			}
			effective["ssl_insecure"] = value
		}
	}
	for _, option := range []string{"ignore_hosts", "allow_hosts"} {
		if list, ok := effective[option].([]any); ok && len(list) != 0 {
			return fmt.Errorf("mitmproxy %s is not supported with Fast Sandbox credential-bound", option)
		}
	}
	if effective["ssl_insecure"] == true {
		return fmt.Errorf("mitmproxy ssl_insecure is not supported with Fast Sandbox credential-bound")
	}
	return nil
}
