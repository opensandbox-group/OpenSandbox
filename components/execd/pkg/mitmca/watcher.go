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

package mitmca

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	mitmEnabledEnv      = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"
	defaultCAPath       = "/opt/opensandbox/mitmproxy-ca-cert.pem"
	refreshOnlyArg      = "--refresh-mitm-ca-trust"
	defaultPollInterval = 2 * time.Second
	refreshTimeout      = 2 * time.Minute
)

type certificateFingerprint [sha256.Size]byte

type watchConfig struct {
	caPath       string
	pollInterval time.Duration
	refresh      func(context.Context) error
	onError      func(error)
	onRefresh    func(certificateFingerprint)
	onReady      func()
}

// Start launches the agent-side mitmproxy CA watcher when transparent MITM is
// enabled. The watcher runs inside execd's context and therefore cannot outlive
// the daemon.
func Start(ctx context.Context) {
	if runtime.GOOS == "windows" || !isTruthy(os.Getenv(mitmEnabledEnv)) {
		return
	}

	executable, err := os.Executable()
	if err != nil {
		log.Warn("mitm_ca_watch status=disabled error=%v", err)
		return
	}
	bootstrapPath := filepath.Join(filepath.Dir(executable), "bootstrap.sh")
	if info, statErr := os.Stat(bootstrapPath); statErr != nil || info.IsDir() {
		log.Warn("mitm_ca_watch status=disabled bootstrap=%s error=%v", bootstrapPath, statErr)
		return
	}

	log.Info("mitm_ca_watch status=started path=%s interval=%s", defaultCAPath, defaultPollInterval)
	go watch(ctx, watchConfig{
		caPath:       defaultCAPath,
		pollInterval: defaultPollInterval,
		refresh: func(refreshCtx context.Context) error {
			commandCtx, cancel := context.WithTimeout(refreshCtx, refreshTimeout)
			defer cancel()
			output, refreshErr := exec.CommandContext(commandCtx, "/bin/sh", bootstrapPath, refreshOnlyArg).CombinedOutput()
			if refreshErr != nil {
				return fmt.Errorf("bootstrap refresh failed: %w output=%q", refreshErr, compactOutput(output))
			}
			return nil
		},
		onError: func(watchErr error) {
			log.Warn("mitm_ca_watch status=waiting error=%v", watchErr)
		},
		onRefresh: func(fingerprint certificateFingerprint) {
			log.Info("mitm_ca_watch status=refreshed fingerprint_sha256=%x", fingerprint)
		},
	})
}

func watch(ctx context.Context, cfg watchConfig) {
	if cfg.pollInterval <= 0 {
		panic("mitmca watcher requires a positive poll interval")
	}

	trusted, hasTrusted, err := readCertificateFingerprint(cfg.caPath)
	lastError := reportChangedError("", err, cfg.onError)
	if cfg.onReady != nil {
		cfg.onReady()
	}

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		candidate, available, readErr := readCertificateFingerprint(cfg.caPath)
		if readErr != nil {
			lastError = reportChangedError(lastError, readErr, cfg.onError)
			continue
		}
		if !available {
			lastError = ""
			continue
		}
		if hasTrusted && candidate == trusted {
			lastError = ""
			continue
		}

		if refreshErr := cfg.refresh(ctx); refreshErr != nil {
			lastError = reportChangedError(lastError, refreshErr, cfg.onError)
			continue
		}

		current, currentAvailable, currentErr := readCertificateFingerprint(cfg.caPath)
		switch {
		case currentErr != nil:
			lastError = reportChangedError(lastError, currentErr, cfg.onError)
			continue
		case !currentAvailable:
			lastError = reportChangedError(lastError, errors.New("CA disappeared during trust refresh"), cfg.onError)
			continue
		case current != candidate:
			lastError = reportChangedError(lastError, errors.New("CA changed during trust refresh"), cfg.onError)
			continue
		}

		trusted = candidate
		hasTrusted = true
		lastError = ""
		if cfg.onRefresh != nil {
			cfg.onRefresh(candidate)
		}
	}
}

func readCertificateFingerprint(path string) (certificateFingerprint, bool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return certificateFingerprint{}, false, nil
		}
		return certificateFingerprint{}, false, fmt.Errorf("read CA at %s: %w", path, err)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return certificateFingerprint{}, false, nil
	}

	block, _ := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" {
		return certificateFingerprint{}, false, fmt.Errorf("parse CA at %s: no PEM certificate found", path)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certificateFingerprint{}, false, fmt.Errorf("parse CA at %s: %w", path, err)
	}
	if !certificate.IsCA {
		return certificateFingerprint{}, false, fmt.Errorf("parse CA at %s: certificate is not a CA", path)
	}
	return sha256.Sum256(certificate.Raw), true, nil
}

func reportChangedError(previous string, err error, report func(error)) string {
	if err == nil {
		return ""
	}
	current := err.Error()
	if current != previous && report != nil {
		report(err)
	}
	return current
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func compactOutput(output []byte) string {
	return strings.Join(strings.Fields(string(output)), " ")
}
