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

// Package cabundle manages runtime-delivered custom CA certificates inside a
// sandbox guest. Inspired by E2B envd's cabundle reconciler, it records owned
// certificate state atomically, avoids slow distro rehash scripts on the
// startup hot path by publishing a combined PEM bundle directly from a
// preserved base system bundle, and cleans up retired managed certificates on
// subsequent /internal/init calls.
package cabundle

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/log"
)

const (
	// DefaultStateDir holds the runtime CA state, pristine base bundle copy,
	// and published combined/extra bundles under /opt/opensandbox (matching
	// bootstrap.sh's CA paths and Landlock's read-only /opt protection for
	// user workloads).
	DefaultStateDir = "/opt/opensandbox/ca"

	// DefaultTrustAnchorDir is the Debian/Ubuntu/Alpine local CA directory
	// where individual managed certificate files are reconciled when present.
	DefaultTrustAnchorDir = "/usr/local/share/ca-certificates"

	managedCertPrefix       = "opensandbox-"
	managedCertSuffix       = ".crt"
	pemBlockTypeCertificate = "CERTIFICATE"

	stateFileName      = "state.json"
	baseBundleFileName = "base-bundle.pem"
	bundleFileName     = "bundle.pem"
	extraFileName      = "extra.pem"

	// MaxBundleBytes bounds the raw PEM payload accepted via /internal/init.
	MaxBundleBytes = 512 << 10 // 512 KiB
	// MaxBundleCerts bounds the number of certificates in a single bundle.
	MaxBundleCerts = 64

	stateSchemaVersion = 1
)

// defaultSystemBundlePaths lists well-known Linux system CA bundle locations
// checked in order when locating the guest's active trust bundle.
var defaultSystemBundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu/Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL/CentOS/Fedora
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/ssl/cert.pem",                  // Alpine/BSD fallback
}

// defaultPreinstalledCertPaths lists single-CA files populated by bootstrap.sh
// or template baking. When /internal/init delivers the exact same certificate
// set, execd has no prior managed state, and the active bundle already contains
// those certificates, Reconcile skips all file writes.
var defaultPreinstalledCertPaths = []string{
	"/opt/opensandbox/mitmproxy-ca-cert.pem",
	"/usr/local/share/ca-certificates/opensandbox-mitmproxy-ca.crt",
	"/etc/pki/ca-trust/source/anchors/opensandbox-mitmproxy-ca.pem",
}

const defaultPreinstalledMergedBundlePath = "/opt/opensandbox/merged-ca-certificates.pem"

// Certificate is a validated X.509 CA certificate decoded from a PEM bundle.
type Certificate struct {
	Fingerprint string // lowercase SHA-256 hex of DER bytes
	PEM         []byte // canonical PEM encoding ending with newline
}

// Config configures where the Manager stores state and reconciles trust files.
type Config struct {
	StateDir               string
	SystemBundlePaths      []string
	TrustAnchorDir         string
	PreinstalledCertPaths  []string
	PreinstalledBundlePath string
	// InheritExtraCAPaths lists existing CA files (for example bootstrap.sh's
	// NODE_EXTRA_CA_CERTS) whose certificates are folded into extra.pem and
	// bundle.pem so runtime CA env defaults do not drop pre-existing CAs.
	InheritExtraCAPaths []string
}

// ReconcileOptions customizes reconciliation behavior for a single call.
type ReconcileOptions struct {
	// PreserveBundleFilesOnClear rewrites bundle.pem and extra.pem to base-only
	// content instead of unlinking them when clearing managed CAs, so surviving
	// processes whose env vars still point at those paths keep working TLS.
	PreserveBundleFilesOnClear bool
}

// DefaultConfig returns the production Linux configuration.
func DefaultConfig() Config {
	preinstalledCerts := make([]string, 0, len(defaultPreinstalledCertPaths)+1)
	if envCA := strings.TrimSpace(os.Getenv("OPENSANDBOX_MITM_CA")); envCA != "" {
		preinstalledCerts = append(preinstalledCerts, envCA)
	}
	preinstalledCerts = append(preinstalledCerts, defaultPreinstalledCertPaths...)

	mergedBundle := strings.TrimSpace(os.Getenv("OPENSANDBOX_MERGED_CA_PATH"))
	if mergedBundle == "" {
		mergedBundle = defaultPreinstalledMergedBundlePath
	}

	var inheritExtra []string
	if existingExtra := strings.TrimSpace(os.Getenv("NODE_EXTRA_CA_CERTS")); existingExtra != "" &&
		filepath.Clean(existingExtra) != filepath.Join(DefaultStateDir, extraFileName) {
		inheritExtra = append(inheritExtra, existingExtra)
	}

	return Config{
		StateDir:               DefaultStateDir,
		SystemBundlePaths:      append([]string(nil), defaultSystemBundlePaths...),
		TrustAnchorDir:         DefaultTrustAnchorDir,
		PreinstalledCertPaths:  preinstalledCerts,
		PreinstalledBundlePath: mergedBundle,
		InheritExtraCAPaths:    inheritExtra,
	}
}

// Result describes the published CA bundle artifacts after Reconcile succeeds.
type Result struct {
	BundlePath   string
	ExtraPath    string
	Fingerprints []string
	Warnings     []string
}

type persistedState struct {
	Version            int      `json:"version"`
	SystemBundlePath   string   `json:"systemBundlePath,omitempty"`
	SystemBundleSynced bool     `json:"systemBundleSynced,omitempty"`
	AnchorsSynced      bool     `json:"anchorsSynced,omitempty"`
	OwnedCertFiles     []string `json:"ownedCertFiles,omitempty"`
	Fingerprints       []string `json:"fingerprints,omitempty"`
}

// Manager serializes CA bundle reconciliation and tracks owned certificate files.
type Manager struct {
	cfg Config
	mu  sync.Mutex

	lastValid  bool
	lastResult Result
}

// NewManager creates a Manager with the given Config, filling in defaults for
// unset fields.
func NewManager(cfg Config) *Manager {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.SystemBundlePaths == nil {
		cfg.SystemBundlePaths = append([]string(nil), defaultSystemBundlePaths...)
	}
	if cfg.TrustAnchorDir == "" {
		cfg.TrustAnchorDir = DefaultTrustAnchorDir
	}
	return &Manager{cfg: cfg}
}

// ParsePEMBundle validates and deduplicates a PEM-encoded CA bundle.
// Every certificate must be a valid, non-expired X.509 CA certificate.
// An empty or whitespace-only string returns an empty slice and nil error,
// which instructs Reconcile to remove all previously managed certificates.
func ParsePEMBundle(raw string) ([]Certificate, error) {
	if len(raw) > MaxBundleBytes {
		return nil, fmt.Errorf("caBundle exceeds maximum size of %d bytes", MaxBundleBytes)
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	now := time.Now()
	var (
		certs []Certificate
		seen  = make(map[string]struct{})
		rest  = []byte(trimmed)
	)
	for {
		rest = bytes.TrimSpace(rest)
		if len(rest) == 0 {
			break
		}
		block, next := pem.Decode(rest)
		if block == nil {
			return nil, errors.New("caBundle contains invalid PEM data")
		}
		if block.Type != pemBlockTypeCertificate {
			return nil, fmt.Errorf("caBundle contains unsupported PEM block type %q", block.Type)
		}
		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("caBundle contains invalid X.509 certificate: %w", err)
		}
		if err := validateCACertificate(parsed, now); err != nil {
			return nil, err
		}
		if len(certs) >= MaxBundleCerts {
			return nil, fmt.Errorf("caBundle must not exceed %d certificates", MaxBundleCerts)
		}

		sum := sha256.Sum256(parsed.Raw)
		fp := hex.EncodeToString(sum[:])
		if _, dup := seen[fp]; !dup {
			seen[fp] = struct{}{}
			canonical := pem.EncodeToMemory(&pem.Block{
				Type:  pemBlockTypeCertificate,
				Bytes: parsed.Raw,
			})
			certs = append(certs, Certificate{
				Fingerprint: fp,
				PEM:         canonical,
			})
		}
		rest = next
	}
	return certs, nil
}

func validateCACertificate(cert *x509.Certificate, now time.Time) error {
	if !cert.IsCA {
		return fmt.Errorf("caBundle certificate %q is not a CA (BasicConstraints IsCA=false)", cert.Subject.CommonName)
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("caBundle certificate %q lacks keyCertSign key usage", cert.Subject.CommonName)
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("caBundle certificate %q is not valid until %s", cert.Subject.CommonName, cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("caBundle certificate %q expired at %s", cert.Subject.CommonName, cert.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

// LoadActive restores the previously committed CA bundle metadata from disk
// (used when /internal/init omits caBundle after an execd restart). State-file
// errors degrade to a warning with empty metadata so callers that omit
// caBundle are never blocked by a corrupt or unsupported state file.
func (m *Manager) LoadActive() (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lastValid {
		return cloneResult(m.lastResult), nil
	}

	st, exists, err := m.loadState()
	if err != nil {
		log.Warn("cabundle: failed to load persisted state on LoadActive: %v", err)
		return Result{
			Warnings: []string{fmt.Sprintf("caBundle: ignored unreadable state file (%v)", err)},
		}, nil
	}
	if !exists || len(st.Fingerprints) == 0 {
		return Result{}, nil
	}

	bundlePath := filepath.Join(m.cfg.StateDir, bundleFileName)
	extraPath := filepath.Join(m.cfg.StateDir, extraFileName)
	if !fileNonEmpty(bundlePath) || !fileNonEmpty(extraPath) {
		return Result{}, nil
	}

	res := Result{
		BundlePath:   bundlePath,
		ExtraPath:    extraPath,
		Fingerprints: append([]string(nil), st.Fingerprints...),
	}
	if stateConverged(st) {
		m.setLastResult(res)
	} else {
		res.Warnings = append(res.Warnings, "caBundle: persisted state is not fully converged; retry with caBundle to finish reconciliation")
	}
	return cloneResult(res), nil
}

// Reconcile atomically installs the desired certificates and removes any
// retired managed certificates from earlier reconciliations. When the desired
// certificate fingerprints already match the active state (or a verified
// pre-installed bootstrap/baked CA with no managed drift), Reconcile returns
// immediately without writing any files.
func (m *Manager) Reconcile(certs []Certificate) (Result, error) {
	return m.ReconcileWithOptions(certs, ReconcileOptions{})
}

// ReconcileWithOptions performs Reconcile with caller-specified options.
func (m *Manager) ReconcileWithOptions(certs []Certificate, opts ReconcileOptions) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fingerprints := collectFingerprints(certs)
	if res, ok := m.tryMemoryFastPath(fingerprints, opts); ok {
		return res, nil
	}

	var loadWarnings []string
	prevState, stateExists, err := m.loadState()
	if err != nil {
		log.Warn("cabundle: ignoring unreadable state file during reconcile: %v", err)
		loadWarnings = append(loadWarnings, fmt.Sprintf("caBundle: ignored unreadable state file (%v)", err))
		prevState = persistedState{Version: stateSchemaVersion}
		stateExists = false
	}

	if len(loadWarnings) == 0 {
		if res, ok := m.tryDiskOrPreinstalledFastPath(prevState, stateExists, fingerprints, opts); ok {
			return res, nil
		}
	}

	res, err := m.reconcileSlowPath(prevState, certs, fingerprints, opts)
	if err != nil {
		return Result{}, err
	}
	if len(loadWarnings) > 0 {
		res.Warnings = append(loadWarnings, res.Warnings...)
	}
	return res, nil
}

func stateConverged(st persistedState) bool {
	if st.SystemBundlePath != "" && !st.SystemBundleSynced {
		return false
	}
	return st.AnchorsSynced
}

func (m *Manager) tryMemoryFastPath(fingerprints []string, opts ReconcileOptions) (Result, bool) {
	if !m.lastValid || len(m.lastResult.Warnings) > 0 || !slices.Equal(m.lastResult.Fingerprints, fingerprints) {
		return Result{}, false
	}
	if len(fingerprints) == 0 {
		if !opts.PreserveBundleFilesOnClear && m.hasPublishedBundleFiles() {
			return Result{}, false
		}
		return cloneResult(m.lastResult), true
	}
	if fileNonEmpty(m.lastResult.BundlePath) && fileNonEmpty(m.lastResult.ExtraPath) {
		return cloneResult(m.lastResult), true
	}
	return Result{}, false
}

func (m *Manager) tryDiskOrPreinstalledFastPath(prevState persistedState, stateExists bool, fingerprints []string, opts ReconcileOptions) (Result, bool) {
	bundlePath := filepath.Join(m.cfg.StateDir, bundleFileName)
	extraPath := filepath.Join(m.cfg.StateDir, extraFileName)

	// Fast path 2: identical fingerprint set already committed cleanly on disk.
	if stateExists && slices.Equal(prevState.Fingerprints, fingerprints) && stateConverged(prevState) {
		if len(fingerprints) == 0 {
			if !opts.PreserveBundleFilesOnClear && m.hasPublishedBundleFiles() {
				return Result{}, false
			}
			m.setLastResult(Result{})
			return Result{}, true
		}
		if bundleContainsAllFingerprints(bundlePath, fingerprints) && bundleContainsAllFingerprints(extraPath, fingerprints) {
			res := Result{
				BundlePath:   bundlePath,
				ExtraPath:    extraPath,
				Fingerprints: fingerprints,
			}
			m.setLastResult(res)
			return cloneResult(res), true
		}
	}

	// Fast path 3: bootstrap.sh or template baking already installed the exact
	// same certificate(s) AND the target bundle actually contains them.
	if len(fingerprints) > 0 && len(prevState.OwnedCertFiles) == 0 {
		if matchedCertPath := m.matchPreinstalledCerts(fingerprints); matchedCertPath != "" {
			if existingBundle := m.detectVerifiedPreinstalledBundle(prevState.SystemBundlePath, fingerprints); existingBundle != "" {
				res := Result{
					BundlePath:   existingBundle,
					ExtraPath:    matchedCertPath,
					Fingerprints: fingerprints,
				}
				m.setLastResult(res)
				return cloneResult(res), true
			}
		}
	}
	return Result{}, false
}

func (m *Manager) reconcileSlowPath(prevState persistedState, certs []Certificate, fingerprints []string, opts ReconcileOptions) (Result, error) {
	if err := os.MkdirAll(m.cfg.StateDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create ca state dir: %w", err)
	}

	systemBundlePath := prevState.SystemBundlePath
	if systemBundlePath == "" {
		systemBundlePath = m.detectSystemBundle()
	}

	baseBundle, err := m.ensureBaseBundle(systemBundlePath)
	if err != nil {
		return Result{}, fmt.Errorf("capture base ca bundle: %w", err)
	}

	desiredFiles, desiredSet := buildDesiredCertFiles(certs)

	// Record intent (union of previous and desired owned files) before mutating
	// trust files so an interrupted reconcile can clean up on retry.
	if err := m.saveState(persistedState{
		Version:            stateSchemaVersion,
		SystemBundlePath:   systemBundlePath,
		SystemBundleSynced: false,
		AnchorsSynced:      false,
		OwnedCertFiles:     unionStrings(prevState.OwnedCertFiles, desiredFiles),
		Fingerprints:       prevState.Fingerprints,
	}); err != nil {
		return Result{}, fmt.Errorf("persist ca intent state: %w", err)
	}

	ownedAfter, anchorsSynced, anchorWarnings := m.reconcileTrustAnchorDir(certs, prevState.OwnedCertFiles, desiredSet)
	result, systemSynced, bundleWarnings, err := m.publishBundles(certs, fingerprints, baseBundle, systemBundlePath, opts)
	if err != nil {
		return Result{}, err
	}

	result.Warnings = append(anchorWarnings, bundleWarnings...)

	committed := persistedState{
		Version:            stateSchemaVersion,
		SystemBundlePath:   systemBundlePath,
		SystemBundleSynced: systemSynced,
		AnchorsSynced:      anchorsSynced,
		OwnedCertFiles:     ownedAfter,
		Fingerprints:       fingerprints,
	}
	if err := m.saveState(committed); err != nil {
		return Result{}, fmt.Errorf("commit ca state: %w", err)
	}

	if stateConverged(committed) && len(result.Warnings) == 0 {
		m.setLastResult(result)
	} else {
		m.lastValid = false
		m.lastResult = Result{}
	}
	return cloneResult(result), nil
}

func (m *Manager) publishBundles(certs []Certificate, fingerprints []string, baseBundle []byte, systemBundlePath string, opts ReconcileOptions) (Result, bool, []string, error) {
	bundlePath := filepath.Join(m.cfg.StateDir, bundleFileName)
	extraPath := filepath.Join(m.cfg.StateDir, extraFileName)
	var warnings []string

	if len(certs) == 0 {
		if err := m.clearPublishedBundles(bundlePath, extraPath, baseBundle, opts.PreserveBundleFilesOnClear); err != nil {
			return Result{}, false, nil, err
		}
		systemSynced := true
		if systemBundlePath != "" && len(baseBundle) > 0 {
			if err := writeFileAtomic(systemBundlePath, baseBundle, 0o644); err != nil {
				log.Warn("cabundle: restore system bundle %s failed: %v", systemBundlePath, err)
				warnings = append(warnings, fmt.Sprintf("caBundle: failed to restore system bundle %s", systemBundlePath))
				systemSynced = false
			}
		}
		return Result{}, systemSynced, warnings, nil
	}

	inheritedExtra := m.loadInheritedExtraCerts(fingerprints)
	extraBytes := mergeBundles(inheritedExtra, concatPEM(certs))
	combinedBytes := mergeBundles(baseBundle, extraBytes)

	if err := writeFileAtomic(extraPath, extraBytes, 0o644); err != nil {
		return Result{}, false, nil, fmt.Errorf("write extra ca bundle: %w", err)
	}
	if err := writeFileAtomic(bundlePath, combinedBytes, 0o644); err != nil {
		return Result{}, false, nil, fmt.Errorf("write combined ca bundle: %w", err)
	}

	systemSynced := true
	if systemBundlePath != "" {
		if err := writeFileAtomic(systemBundlePath, combinedBytes, 0o644); err != nil {
			log.Warn("cabundle: update system bundle %s failed: %v", systemBundlePath, err)
			warnings = append(warnings, fmt.Sprintf("caBundle: failed to update system bundle %s; only CA env vars point to the merged bundle", systemBundlePath))
			systemSynced = false
		}
	}

	return Result{
		BundlePath:   bundlePath,
		ExtraPath:    extraPath,
		Fingerprints: fingerprints,
	}, systemSynced, warnings, nil
}

func (m *Manager) clearPublishedBundles(bundlePath, extraPath string, baseBundle []byte, preserveFiles bool) error {
	if !preserveFiles {
		if err := removeIfExists(extraPath); err != nil {
			return fmt.Errorf("remove extra ca bundle: %w", err)
		}
		if err := removeIfExists(bundlePath); err != nil {
			return fmt.Errorf("remove combined ca bundle: %w", err)
		}
		return nil
	}

	// PreserveRuntimeState clear: keep bundle files on disk with base-only
	// content so surviving processes whose env vars still reference them do not
	// lose TLS verification.
	inheritedExtra := m.loadInheritedExtraCerts(nil)
	if fileExists(extraPath) {
		if err := writeFileAtomic(extraPath, inheritedExtra, 0o644); err != nil {
			return fmt.Errorf("reset extra ca bundle: %w", err)
		}
	}
	if fileExists(bundlePath) {
		if err := writeFileAtomic(bundlePath, mergeBundles(baseBundle, inheritedExtra), 0o644); err != nil {
			return fmt.Errorf("reset combined ca bundle: %w", err)
		}
	}
	return nil
}

func (m *Manager) hasPublishedBundleFiles() bool {
	return fileExists(filepath.Join(m.cfg.StateDir, bundleFileName)) ||
		fileExists(filepath.Join(m.cfg.StateDir, extraFileName))
}

func (m *Manager) loadInheritedExtraCerts(excludeFingerprints []string) []byte {
	if len(m.cfg.InheritExtraCAPaths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(excludeFingerprints))
	for _, fp := range excludeFingerprints {
		seen[fp] = struct{}{}
	}
	ownExtra := filepath.Clean(filepath.Join(m.cfg.StateDir, extraFileName))
	ownBundle := filepath.Clean(filepath.Join(m.cfg.StateDir, bundleFileName))

	var buf bytes.Buffer
	for _, p := range m.cfg.InheritExtraCAPaths {
		cleaned := filepath.Clean(strings.TrimSpace(p))
		if cleaned == "" || cleaned == "." || cleaned == ownExtra || cleaned == ownBundle {
			continue
		}
		data, err := os.ReadFile(cleaned)
		if err != nil || len(data) == 0 || len(data) > MaxBundleBytes {
			continue
		}
		appendUniquePEMCerts(&buf, data, seen)
	}
	return buf.Bytes()
}

func appendUniquePEMCerts(dst *bytes.Buffer, data []byte, seen map[string]struct{}) {
	rest := data
	for len(rest) > 0 {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == pemBlockTypeCertificate {
			sum := sha256.Sum256(block.Bytes)
			fp := hex.EncodeToString(sum[:])
			if _, dup := seen[fp]; !dup {
				seen[fp] = struct{}{}
				dst.Write(pem.EncodeToMemory(&pem.Block{
					Type:  pemBlockTypeCertificate,
					Bytes: block.Bytes,
				}))
			}
		}
		rest = next
	}
}

func (m *Manager) detectSystemBundle() string {
	for _, p := range m.cfg.SystemBundlePaths {
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func (m *Manager) ensureBaseBundle(systemBundlePath string) ([]byte, error) {
	basePath := filepath.Join(m.cfg.StateDir, baseBundleFileName)
	data, err := os.ReadFile(basePath)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if systemBundlePath == "" {
		return nil, nil
	}
	sysData, err := os.ReadFile(systemBundlePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if err := writeFileAtomic(basePath, sysData, 0o644); err != nil {
		return nil, err
	}
	return sysData, nil
}

// reconcileTrustAnchorDir updates managed certificate files in TrustAnchorDir.
// It returns the full set of owned filenames to persist (written + retained
// failed removals), whether the anchor directory converged cleanly, and any
// non-fatal warnings.
func (m *Manager) reconcileTrustAnchorDir(certs []Certificate, prevOwned []string, desiredSet map[string]struct{}) ([]string, bool, []string) {
	if m.cfg.TrustAnchorDir == "" {
		return nil, true, nil
	}
	info, err := os.Stat(m.cfg.TrustAnchorDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if len(certs) > 0 {
				log.Warn("cabundle: trust anchor dir %s does not exist; skipping individual anchor files", m.cfg.TrustAnchorDir)
				return prevOwned, len(prevOwned) == 0, []string{
					fmt.Sprintf("caBundle: trust anchor dir %s does not exist; skipped individual anchor files", m.cfg.TrustAnchorDir),
				}
			}
			return prevOwned, len(prevOwned) == 0, nil
		}
		log.Warn("cabundle: stat trust anchor dir %s: %v", m.cfg.TrustAnchorDir, err)
		return prevOwned, false, []string{
			fmt.Sprintf("caBundle: failed to inspect trust anchor dir %s", m.cfg.TrustAnchorDir),
		}
	}
	if !info.IsDir() {
		log.Warn("cabundle: trust anchor path %s is not a directory", m.cfg.TrustAnchorDir)
		return prevOwned, false, []string{
			fmt.Sprintf("caBundle: trust anchor path %s is not a directory", m.cfg.TrustAnchorDir),
		}
	}

	var (
		owned    []string
		warnings []string
		synced   = true
	)
	for _, cert := range certs {
		name := managedCertPrefix + cert.Fingerprint + managedCertSuffix
		target := filepath.Join(m.cfg.TrustAnchorDir, name)
		if existing, readErr := os.ReadFile(target); readErr == nil && bytes.Equal(existing, cert.PEM) {
			owned = append(owned, name)
			continue
		}
		if writeErr := writeFileAtomic(target, cert.PEM, 0o644); writeErr != nil {
			log.Warn("cabundle: write trust anchor %s failed: %v", target, writeErr)
			warnings = append(warnings, fmt.Sprintf("caBundle: failed to write trust anchor %s", name))
			synced = false
			continue
		}
		owned = append(owned, name)
	}

	for _, oldName := range prevOwned {
		if _, keep := desiredSet[oldName]; keep {
			continue
		}
		if !strings.HasPrefix(oldName, managedCertPrefix) || !strings.HasSuffix(oldName, managedCertSuffix) {
			continue
		}
		base := filepath.Base(oldName)
		if rmErr := removeIfExists(filepath.Join(m.cfg.TrustAnchorDir, base)); rmErr != nil {
			log.Warn("cabundle: remove retired trust anchor %s failed: %v", base, rmErr)
			warnings = append(warnings, fmt.Sprintf("caBundle: failed to remove retired trust anchor %s", base))
			owned = append(owned, base)
			synced = false
		}
	}
	return owned, synced, warnings
}

func (m *Manager) loadState() (persistedState, bool, error) {
	path := filepath.Join(m.cfg.StateDir, stateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return persistedState{Version: stateSchemaVersion}, false, nil
		}
		return persistedState{}, false, err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return persistedState{}, false, err
	}
	if st.Version != stateSchemaVersion {
		return persistedState{}, false, fmt.Errorf("unsupported ca state schema version %d", st.Version)
	}
	// Never trust an arbitrary systemBundlePath from state.json: constrain it
	// to the configured allowlist so state tampering cannot steer writes.
	if st.SystemBundlePath != "" && !slices.Contains(m.cfg.SystemBundlePaths, st.SystemBundlePath) {
		log.Warn("cabundle: ignoring untrusted systemBundlePath %q from state.json", st.SystemBundlePath)
		st.SystemBundlePath = ""
	}
	return st, true, nil
}

func (m *Manager) saveState(st persistedState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(m.cfg.StateDir, stateFileName), data, 0o644)
}

func (m *Manager) setLastResult(res Result) {
	m.lastResult = cloneResult(res)
	m.lastValid = true
}

func (m *Manager) matchPreinstalledCerts(desiredFingerprints []string) string {
	for _, p := range m.cfg.PreinstalledCertPaths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 || len(data) > MaxBundleBytes {
			continue
		}
		existingFPs, ok := extractPEMFingerprints(data)
		if ok && slices.Equal(existingFPs, desiredFingerprints) {
			return p
		}
	}
	return ""
}

func (m *Manager) detectVerifiedPreinstalledBundle(savedSystemBundle string, desiredFingerprints []string) string {
	candidates := make([]string, 0, 3)
	if m.cfg.PreinstalledBundlePath != "" {
		candidates = append(candidates, m.cfg.PreinstalledBundlePath)
	}
	if savedSystemBundle != "" {
		candidates = append(candidates, savedSystemBundle)
	}
	if sys := m.detectSystemBundle(); sys != "" {
		candidates = append(candidates, sys)
	}
	for _, candidate := range candidates {
		if fileNonEmpty(candidate) && bundleContainsAllFingerprints(candidate, desiredFingerprints) {
			return candidate
		}
	}
	return ""
}

func extractPEMFingerprints(data []byte) ([]string, bool) {
	var (
		fps  []string
		seen = make(map[string]struct{})
		rest = bytes.TrimSpace(data)
	)
	for len(rest) > 0 {
		block, next := pem.Decode(rest)
		if block == nil || block.Type != pemBlockTypeCertificate {
			return nil, false
		}
		sum := sha256.Sum256(block.Bytes)
		fp := hex.EncodeToString(sum[:])
		if _, dup := seen[fp]; !dup {
			seen[fp] = struct{}{}
			fps = append(fps, fp)
		}
		rest = bytes.TrimSpace(next)
	}
	return fps, len(fps) > 0
}

func bundleContainsAllFingerprints(path string, desired []string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return false
	}
	needed := make(map[string]struct{}, len(desired))
	for _, fp := range desired {
		needed[fp] = struct{}{}
	}
	rest := data
	for len(rest) > 0 && len(needed) > 0 {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == pemBlockTypeCertificate {
			sum := sha256.Sum256(block.Bytes)
			delete(needed, hex.EncodeToString(sum[:]))
		}
		rest = next
	}
	return len(needed) == 0
}

func collectFingerprints(certs []Certificate) []string {
	if len(certs) == 0 {
		return nil
	}
	fps := make([]string, len(certs))
	for i, c := range certs {
		fps[i] = c.Fingerprint
	}
	return fps
}

func buildDesiredCertFiles(certs []Certificate) ([]string, map[string]struct{}) {
	files := make([]string, 0, len(certs))
	set := make(map[string]struct{}, len(certs))
	for _, cert := range certs {
		name := managedCertPrefix + cert.Fingerprint + managedCertSuffix
		files = append(files, name)
		set[name] = struct{}{}
	}
	return files, set
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func fileNonEmpty(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func cloneResult(r Result) Result {
	return Result{
		BundlePath:   r.BundlePath,
		ExtraPath:    r.ExtraPath,
		Fingerprints: append([]string(nil), r.Fingerprints...),
		Warnings:     append([]string(nil), r.Warnings...),
	}
}

func concatPEM(certs []Certificate) []byte {
	var buf bytes.Buffer
	for _, c := range certs {
		buf.Write(c.PEM)
	}
	return buf.Bytes()
}

func mergeBundles(base, extra []byte) []byte {
	if len(base) == 0 {
		return append([]byte(nil), extra...)
	}
	var buf bytes.Buffer
	buf.Grow(len(base) + 1 + len(extra))
	buf.Write(base)
	if base[len(base)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.Write(extra)
	return buf.Bytes()
}

func unionStrings(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opensandbox-ca-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
