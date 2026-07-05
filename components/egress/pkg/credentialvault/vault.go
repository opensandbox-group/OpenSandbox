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

package credentialvault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

const (
	maxCredentialVaultBodyBytes = 1 << 20
	mitmproxyConfigPath         = "/var/lib/mitmproxy/.mitmproxy/config.yaml"
)

var (
	ErrNotFound = errors.New("credential vault not found")
	ErrExists   = errors.New("credential vault already exists")

	headerFieldNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_` + "`" + `|~]+$`)
	hostnameLabelPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	reservedHeaderNames    = map[string]struct{}{
		"host":                {},
		"content-length":      {},
		"content-type":        {},
		"transfer-encoding":   {},
		"connection":          {},
		"upgrade":             {},
		"te":                  {},
		"trailer":             {},
		"proxy-authorization": {},
		"proxy-authenticate":  {},
		"forwarded":           {},
		"x-forwarded-for":     {},
		"x-forwarded-host":    {},
		"x-forwarded-proto":   {},
	}
)

type Store struct {
	mu             sync.RWMutex
	exists         bool
	revision       int64
	credentials    map[string]record
	bindings       map[string]Binding
	interceptPorts map[int]struct{}
	mitmGate       *mitmproxy.HealthGate
	requireToken   func() bool
}

type record struct {
	Name       string
	SourceType string
	Value      string
	Revision   int64
}

type CreateRequest struct {
	Credentials []Credential `json:"credentials"`
	Bindings    []Binding    `json:"bindings"`
}

type MutationRequest struct {
	ExpectedRevision *int64                 `json:"expectedRevision,omitempty"`
	Credentials      *CredentialMutationSet `json:"credentials,omitempty"`
	Bindings         *BindingMutationSet    `json:"bindings,omitempty"`
}

type CredentialMutationSet struct {
	Add     []Credential `json:"add,omitempty"`
	Replace []Credential `json:"replace,omitempty"`
	Delete  []string     `json:"delete,omitempty"`
}

type BindingMutationSet struct {
	Add     []Binding `json:"add,omitempty"`
	Replace []Binding `json:"replace,omitempty"`
	Delete  []string  `json:"delete,omitempty"`
}

type Credential struct {
	Name   string                 `json:"name"`
	Source InlineCredentialSource `json:"source"`
}

type InlineCredentialSource struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type Binding struct {
	Name  string `json:"name"`
	Match Match  `json:"match"`
	Auth  Auth   `json:"auth"`
}

type Match struct {
	Schemes []string `json:"schemes,omitempty"`
	Ports   []int    `json:"ports,omitempty"`
	Hosts   []string `json:"hosts"`
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

type Auth struct {
	Type       string              `json:"type"`
	Credential string              `json:"credential,omitempty"`
	Name       string              `json:"name,omitempty"`
	Headers    []CustomHeaderEntry `json:"headers,omitempty"`
}

type CustomHeaderEntry struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

type State struct {
	Revision    int64             `json:"revision"`
	Credentials []Metadata        `json:"credentials"`
	Bindings    []BindingMetadata `json:"bindings"`
}

type ListResponse struct {
	Revision    int64      `json:"revision"`
	Credentials []Metadata `json:"credentials"`
}

type BindingListResponse struct {
	Revision int64             `json:"revision"`
	Bindings []BindingMetadata `json:"bindings"`
}

type Metadata struct {
	Name       string `json:"name"`
	SourceType string `json:"sourceType"`
	Revision   int64  `json:"revision"`
}

type BindingMetadata struct {
	Name     string       `json:"name"`
	Revision int64        `json:"revision"`
	Match    Match        `json:"match"`
	Auth     AuthMetadata `json:"auth"`
}

type AuthMetadata struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type ActiveSnapshot struct {
	Revision   int64           `json:"revision"`
	Bindings   []ActiveBinding `json:"bindings"`
	Redactions []string        `json:"redactions,omitempty"`
}

type ActiveBinding struct {
	Name    string            `json:"name"`
	Match   Match             `json:"match"`
	Headers []InjectionHeader `json:"headers"`
}

type InjectionHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func NewStore(mitmGate *mitmproxy.HealthGate, requireToken func() bool) *Store {
	return &Store{
		credentials:    make(map[string]record),
		bindings:       make(map[string]Binding),
		interceptPorts: map[int]struct{}{80: {}, 443: {}},
		mitmGate:       mitmGate,
		requireToken:   requireToken,
	}
}

func (v *Store) Create(req CreateRequest, pol *policy.NetworkPolicy) (State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.exists {
		return State{}, ErrExists
	}

	credentials := make(map[string]record, len(req.Credentials))
	bindings := make(map[string]Binding, len(req.Bindings))
	for _, c := range req.Credentials {
		rec, err := normalizeCredential(c, 1)
		if err != nil {
			return State{}, err
		}
		if _, ok := credentials[rec.Name]; ok {
			return State{}, fmt.Errorf("duplicate credential name %q", rec.Name)
		}
		credentials[rec.Name] = rec
	}
	for _, b := range req.Bindings {
		nb, err := normalizeBinding(b)
		if err != nil {
			return State{}, err
		}
		if _, ok := bindings[nb.Name]; ok {
			return State{}, fmt.Errorf("duplicate binding name %q", nb.Name)
		}
		bindings[nb.Name] = nb
	}
	if err := v.validateCandidate(credentials, bindings, pol); err != nil {
		return State{}, err
	}

	v.exists = true
	v.revision = 1
	v.credentials = credentials
	v.bindings = bindings
	return v.sanitizedLocked(), nil
}

func (v *Store) Patch(req MutationRequest, pol *policy.NetworkPolicy) (State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.exists {
		return State{}, ErrNotFound
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision != v.revision {
		return State{}, fmt.Errorf("expectedRevision %d does not match current revision %d", *req.ExpectedRevision, v.revision)
	}

	nextRevision := v.revision + 1
	credentials := cloneCredentialRecords(v.credentials)
	bindings := cloneCredentialBindings(v.bindings)

	if err := applyCredentialMutations(credentials, req.Credentials, nextRevision); err != nil {
		return State{}, err
	}
	if err := applyBindingMutations(bindings, req.Bindings); err != nil {
		return State{}, err
	}
	if err := v.validateCandidate(credentials, bindings, pol); err != nil {
		return State{}, err
	}

	v.revision = nextRevision
	v.credentials = credentials
	v.bindings = bindings
	return v.sanitizedLocked(), nil
}

func (v *Store) Delete() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.exists {
		return ErrNotFound
	}
	v.exists = false
	v.revision = 0
	v.credentials = make(map[string]record)
	v.bindings = make(map[string]Binding)
	return nil
}

func (v *Store) Sanitized() (State, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists {
		return State{}, ErrNotFound
	}
	return v.sanitizedLocked(), nil
}

func (v *Store) sanitizedLocked() State {
	state := State{
		Revision:    v.revision,
		Credentials: make([]Metadata, 0, len(v.credentials)),
		Bindings:    make([]BindingMetadata, 0, len(v.bindings)),
	}
	for _, c := range v.credentials {
		state.Credentials = append(state.Credentials, Metadata{
			Name:       c.Name,
			SourceType: c.SourceType,
			Revision:   c.Revision,
		})
	}
	for _, b := range v.bindings {
		state.Bindings = append(state.Bindings, BindingMetadata{
			Name:     b.Name,
			Revision: v.revision,
			Match:    b.Match,
			Auth:     sanitizeAuth(b.Auth),
		})
	}
	sort.Slice(state.Credentials, func(i, j int) bool { return state.Credentials[i].Name < state.Credentials[j].Name })
	sort.Slice(state.Bindings, func(i, j int) bool { return state.Bindings[i].Name < state.Bindings[j].Name })
	return state
}

func (v *Store) ActiveSnapshot() (ActiveSnapshot, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists {
		return ActiveSnapshot{}, ErrNotFound
	}
	snapshot := ActiveSnapshot{
		Revision: v.revision,
		Bindings: make([]ActiveBinding, 0, len(v.bindings)),
	}
	redactions := make(map[string]struct{})
	names := make([]string, 0, len(v.bindings))
	for name := range v.bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b := v.bindings[name]
		headers, values, err := renderInjectionHeaders(b.Auth, v.credentials)
		if err != nil {
			return ActiveSnapshot{}, err
		}
		snapshot.Bindings = append(snapshot.Bindings, ActiveBinding{
			Name:    b.Name,
			Match:   b.Match,
			Headers: headers,
		})
		for _, value := range values {
			if value != "" {
				redactions[value] = struct{}{}
			}
		}
	}
	for value := range redactions {
		snapshot.Redactions = append(snapshot.Redactions, value)
	}
	sort.Strings(snapshot.Redactions)
	return snapshot, nil
}

func (v *Store) ValidateActiveAgainstPolicy(pol *policy.NetworkPolicy) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists || len(v.bindings) == 0 {
		return nil
	}
	return v.validateCandidate(v.credentials, v.bindings, pol)
}

func (v *Store) Ready(ctx context.Context) error {
	if v.requireToken != nil && !v.requireToken() {
		return fmt.Errorf("credential vault requires egress API auth token")
	}
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return fmt.Errorf("credential vault requires transparent mitmproxy")
	}
	if constants.IsTruthy(os.Getenv(constants.EnvMitmproxySslInsecure)) {
		return fmt.Errorf("credential vault rejects insecure upstream TLS mode")
	}
	if !constants.ModeUsesNft(os.Getenv(constants.EnvEgressMode)) {
		return fmt.Errorf("credential vault requires dns+nft egress enforcement")
	}
	if v.mitmGate != nil && !v.mitmGate.WaitReady(ctx) {
		return fmt.Errorf("credential proxy is not ready")
	}
	return nil
}

func (v *Store) validateCandidate(credentials map[string]record, bindings map[string]Binding, pol *policy.NetworkPolicy) error {
	if len(bindings) > 0 && pol == nil {
		return fmt.Errorf("credential vault bindings require an egress policy")
	}
	if len(bindings) > 0 && pol.DefaultAction != policy.ActionDeny {
		log.Warnf("credential vault: default-allow egress policy is deprecated and may allow credential destination bypass; use defaultAction=deny")
	}
	for _, b := range bindings {
		if err := validateBindingCredentialRefs(b, credentials); err != nil {
			return err
		}
		if err := v.validateBindingPolicy(b, pol); err != nil {
			return err
		}
	}
	if err := validateBindingAmbiguity(bindings); err != nil {
		return err
	}
	return nil
}

func (v *Store) validateBindingPolicy(b Binding, pol *policy.NetworkPolicy) error {
	for _, port := range b.Match.Ports {
		if _, ok := v.interceptPorts[port]; !ok {
			return fmt.Errorf("binding %q port %d is not a configured transparent intercept port", b.Name, port)
		}
	}
	for _, host := range b.Match.Hosts {
		if !explicitAllowCoversHost(pol, host) {
			return fmt.Errorf("binding %q host %q is not allowed by egress policy", b.Name, host)
		}
		if bindingHostMatchesIgnoreHosts(host) {
			return fmt.Errorf("binding %q host %q matches mitmproxy ignore_hosts", b.Name, host)
		}
	}
	return nil
}

func normalizeCredential(c Credential, revision int64) (record, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return record{}, fmt.Errorf("credential name cannot be blank")
	}
	sourceType := strings.TrimSpace(c.Source.Type)
	if sourceType == "" {
		sourceType = "inline"
	}
	if sourceType != "inline" {
		return record{}, fmt.Errorf("unsupported credential source type %q", sourceType)
	}
	if c.Source.Value == "" {
		return record{}, fmt.Errorf("credential %q inline value cannot be empty", name)
	}
	return record{Name: name, SourceType: sourceType, Value: c.Source.Value, Revision: revision}, nil
}

func normalizeBinding(b Binding) (Binding, error) {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return Binding{}, fmt.Errorf("binding name cannot be blank")
	}
	if err := normalizeMatch(&b.Match); err != nil {
		return Binding{}, fmt.Errorf("binding %q: %w", b.Name, err)
	}
	if err := normalizeAuth(&b.Auth); err != nil {
		return Binding{}, fmt.Errorf("binding %q: %w", b.Name, err)
	}
	return b, nil
}

func normalizeMatch(m *Match) error {
	if len(m.Schemes) == 0 {
		m.Schemes = []string{"https"}
	}
	if len(m.Ports) == 0 {
		m.Ports = []int{443}
	}
	if len(m.Methods) == 0 {
		m.Methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	}
	if len(m.Paths) == 0 {
		m.Paths = []string{"/*"}
	}
	if len(m.Hosts) == 0 {
		return fmt.Errorf("match.hosts cannot be empty")
	}

	for i, scheme := range m.Schemes {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme != "https" && scheme != "http" {
			return fmt.Errorf("unsupported scheme %q", scheme)
		}
		m.Schemes[i] = scheme
	}
	for _, port := range m.Ports {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("invalid port %d", port)
		}
	}
	for i, host := range m.Hosts {
		normalized, err := normalizeCredentialHost(host)
		if err != nil {
			return err
		}
		m.Hosts[i] = normalized
	}
	for i, method := range m.Methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			return fmt.Errorf("method cannot be blank")
		}
		m.Methods[i] = method
	}
	for i, path := range m.Paths {
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasPrefix(path, "/") {
			return fmt.Errorf("path pattern must start with /")
		}
		m.Paths[i] = path
	}
	dedupeStringsInPlace(&m.Schemes)
	dedupeIntsInPlace(&m.Ports)
	dedupeStringsInPlace(&m.Hosts)
	dedupeStringsInPlace(&m.Methods)
	dedupeStringsInPlace(&m.Paths)
	return nil
}

func normalizeAuth(a *Auth) error {
	a.Type = strings.TrimSpace(a.Type)
	switch a.Type {
	case "bearer", "basic":
		a.Credential = strings.TrimSpace(a.Credential)
		if a.Credential == "" {
			return fmt.Errorf("%s auth requires credential", a.Type)
		}
	case "apiKey":
		a.Name = canonicalHeaderName(strings.TrimSpace(a.Name))
		if err := validateCredentialHeaderName(a.Name); err != nil {
			return err
		}
		a.Credential = strings.TrimSpace(a.Credential)
		if a.Credential == "" {
			return fmt.Errorf("%s auth requires credential", a.Type)
		}
	case "customHeaders":
		if len(a.Headers) == 0 {
			return fmt.Errorf("customHeaders auth requires headers")
		}
		seen := make(map[string]struct{}, len(a.Headers))
		for i := range a.Headers {
			h := &a.Headers[i]
			h.Name = canonicalHeaderName(strings.TrimSpace(h.Name))
			if err := validateCredentialHeaderName(h.Name); err != nil {
				return err
			}
			key := strings.ToLower(h.Name)
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate custom header name %q", h.Name)
			}
			seen[key] = struct{}{}
			h.Credential = strings.TrimSpace(h.Credential)
			if h.Credential == "" {
				return fmt.Errorf("customHeaders entry %q requires credential", h.Name)
			}
		}
	default:
		return fmt.Errorf("unsupported auth type %q", a.Type)
	}
	return nil
}

func validateCredentialHeaderName(name string) error {
	if name == "" || !headerFieldNamePattern.MatchString(name) {
		return fmt.Errorf("invalid credential header name %q", name)
	}
	if _, denied := reservedHeaderNames[strings.ToLower(name)]; denied {
		return fmt.Errorf("reserved credential header name %q", name)
	}
	return nil
}

func validateBindingCredentialRefs(b Binding, credentials map[string]record) error {
	for _, name := range credentialRefsForAuth(b.Auth) {
		if _, ok := credentials[name]; !ok {
			return fmt.Errorf("binding %q references unknown credential %q", b.Name, name)
		}
	}
	return nil
}

func credentialRefsForAuth(auth Auth) []string {
	if auth.Type == "customHeaders" {
		out := make([]string, 0, len(auth.Headers))
		for _, h := range auth.Headers {
			out = append(out, h.Credential)
		}
		return out
	}
	return []string{auth.Credential}
}

func renderInjectionHeaders(auth Auth, credentials map[string]record) ([]InjectionHeader, []string, error) {
	valueFor := func(name string) (string, error) {
		c, ok := credentials[name]
		if !ok {
			return "", fmt.Errorf("unknown credential %q", name)
		}
		return c.Value, nil
	}
	var headers []InjectionHeader
	var redactions []string
	switch auth.Type {
	case "bearer":
		value, err := valueFor(auth.Credential)
		if err != nil {
			return nil, nil, err
		}
		rendered := "Bearer " + value
		headers = append(headers, InjectionHeader{Name: "Authorization", Value: rendered})
		redactions = append(redactions, value, rendered)
	case "basic":
		value, err := valueFor(auth.Credential)
		if err != nil {
			return nil, nil, err
		}
		rendered := "Basic " + value
		headers = append(headers, InjectionHeader{Name: "Authorization", Value: rendered})
		redactions = append(redactions, value, rendered)
	case "apiKey":
		value, err := valueFor(auth.Credential)
		if err != nil {
			return nil, nil, err
		}
		headers = append(headers, InjectionHeader{Name: auth.Name, Value: value})
		redactions = append(redactions, value)
	case "customHeaders":
		for _, h := range auth.Headers {
			value, err := valueFor(h.Credential)
			if err != nil {
				return nil, nil, err
			}
			headers = append(headers, InjectionHeader{Name: h.Name, Value: value})
			redactions = append(redactions, value)
		}
	default:
		return nil, nil, fmt.Errorf("unsupported auth type %q", auth.Type)
	}
	return headers, redactions, nil
}

func sanitizeAuth(auth Auth) AuthMetadata {
	meta := AuthMetadata{Type: auth.Type}
	switch auth.Type {
	case "apiKey":
		meta.Name = auth.Name
	}
	return meta
}

func applyCredentialMutations(credentials map[string]record, mutations *CredentialMutationSet, revision int64) error {
	if mutations == nil {
		return nil
	}
	mentioned := make(map[string]struct{})
	for _, name := range mutations.Delete {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("credential delete name cannot be blank")
		}
		if _, duplicate := mentioned[name]; duplicate {
			return fmt.Errorf("credential %q mentioned by multiple operations", name)
		}
		mentioned[name] = struct{}{}
		if _, ok := credentials[name]; !ok {
			return fmt.Errorf("credential %q does not exist", name)
		}
		delete(credentials, name)
	}
	for _, raw := range mutations.Replace {
		rec, err := normalizeCredential(raw, revision)
		if err != nil {
			return err
		}
		if _, duplicate := mentioned[rec.Name]; duplicate {
			return fmt.Errorf("credential %q mentioned by multiple operations", rec.Name)
		}
		mentioned[rec.Name] = struct{}{}
		if _, ok := credentials[rec.Name]; !ok {
			return fmt.Errorf("credential %q does not exist", rec.Name)
		}
		credentials[rec.Name] = rec
	}
	addSeen := make(map[string]struct{})
	for _, raw := range mutations.Add {
		rec, err := normalizeCredential(raw, revision)
		if err != nil {
			return err
		}
		if _, duplicate := mentioned[rec.Name]; duplicate {
			return fmt.Errorf("credential %q mentioned by multiple operations", rec.Name)
		}
		if _, duplicate := addSeen[rec.Name]; duplicate {
			return fmt.Errorf("duplicate credential add name %q", rec.Name)
		}
		addSeen[rec.Name] = struct{}{}
		if _, ok := credentials[rec.Name]; ok {
			return fmt.Errorf("credential %q already exists", rec.Name)
		}
		credentials[rec.Name] = rec
	}
	return nil
}

func applyBindingMutations(bindings map[string]Binding, mutations *BindingMutationSet) error {
	if mutations == nil {
		return nil
	}
	mentioned := make(map[string]struct{})
	for _, name := range mutations.Delete {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("binding delete name cannot be blank")
		}
		if _, duplicate := mentioned[name]; duplicate {
			return fmt.Errorf("binding %q mentioned by multiple operations", name)
		}
		mentioned[name] = struct{}{}
		if _, ok := bindings[name]; !ok {
			return fmt.Errorf("binding %q does not exist", name)
		}
		delete(bindings, name)
	}
	for _, raw := range mutations.Replace {
		b, err := normalizeBinding(raw)
		if err != nil {
			return err
		}
		if _, duplicate := mentioned[b.Name]; duplicate {
			return fmt.Errorf("binding %q mentioned by multiple operations", b.Name)
		}
		mentioned[b.Name] = struct{}{}
		if _, ok := bindings[b.Name]; !ok {
			return fmt.Errorf("binding %q does not exist", b.Name)
		}
		bindings[b.Name] = b
	}
	addSeen := make(map[string]struct{})
	for _, raw := range mutations.Add {
		b, err := normalizeBinding(raw)
		if err != nil {
			return err
		}
		if _, duplicate := mentioned[b.Name]; duplicate {
			return fmt.Errorf("binding %q mentioned by multiple operations", b.Name)
		}
		if _, duplicate := addSeen[b.Name]; duplicate {
			return fmt.Errorf("duplicate binding add name %q", b.Name)
		}
		addSeen[b.Name] = struct{}{}
		if _, ok := bindings[b.Name]; ok {
			return fmt.Errorf("binding %q already exists", b.Name)
		}
		bindings[b.Name] = b
	}
	return nil
}

func cloneCredentialRecords(in map[string]record) map[string]record {
	out := make(map[string]record, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCredentialBindings(in map[string]Binding) map[string]Binding {
	out := make(map[string]Binding, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeCredentialHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return "", fmt.Errorf("host cannot be blank")
	}
	if strings.Contains(host, "://") || strings.Contains(host, "/") {
		return "", fmt.Errorf("host %q must not include scheme or path", host)
	}
	if strings.HasPrefix(host, "*.") {
		suffix := strings.TrimPrefix(host, "*.")
		if suffix == "" || strings.Contains(suffix, "*") {
			return "", fmt.Errorf("invalid wildcard host %q", host)
		}
		if _, err := netip.ParseAddr(suffix); err == nil {
			return "", fmt.Errorf("wildcard host %q cannot target an IP address", host)
		}
		if !isValidCredentialFQDN(suffix) {
			return "", fmt.Errorf("invalid wildcard host %q", host)
		}
		return "*." + suffix, nil
	}
	if strings.Contains(host, "*") {
		return "", fmt.Errorf("invalid wildcard host %q", host)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", fmt.Errorf("credential binding host %q must be an FQDN, not an IP address", host)
	}
	if !isValidCredentialFQDN(host) {
		return "", fmt.Errorf("credential binding host %q must be an FQDN", host)
	}
	return host, nil
}

func isValidCredentialFQDN(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !hostnameLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func explicitAllowCoversHost(pol *policy.NetworkPolicy, host string) bool {
	if pol == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "*.") {
		return pol.Evaluate("probe."+strings.TrimPrefix(host, "*.")) == policy.ActionAllow
	}
	return pol.Evaluate(host) == policy.ActionAllow
}

func bindingHostMatchesIgnoreHosts(host string) bool {
	patterns := parseMitmproxyIgnoreHosts(readMitmproxyConfig(mitmproxyConfigPath))
	if len(patterns) == 0 {
		return false
	}
	candidates := []string{host}
	if strings.HasPrefix(host, "*.") {
		candidates = append(candidates, "probe."+strings.TrimPrefix(host, "*."))
	}
	for _, part := range patterns {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		re, err := regexp.Compile(part)
		if err != nil {
			continue
		}
		for _, candidate := range candidates {
			if re.MatchString(candidate) {
				return true
			}
		}
	}
	return false
}

func readMitmproxyConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseMitmproxyIgnoreHosts(config string) []string {
	lines := strings.Split(config, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(key) != "ignore_hosts" {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return parseMitmproxyInlineList(value)
		}
		var out []string
		for _, itemLine := range lines[i+1:] {
			itemTrimmed := strings.TrimSpace(itemLine)
			if itemTrimmed == "" || strings.HasPrefix(itemTrimmed, "#") {
				continue
			}
			if !strings.HasPrefix(itemLine, " ") && !strings.HasPrefix(itemLine, "\t") {
				break
			}
			if !strings.HasPrefix(itemTrimmed, "-") {
				continue
			}
			item := strings.TrimSpace(strings.TrimPrefix(itemTrimmed, "-"))
			if item != "" {
				out = append(out, trimYAMLScalar(item))
			}
		}
		return out
	}
	return nil
}

func parseMitmproxyInlineList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "[]" {
		return nil
	}
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return []string{trimYAMLScalar(value)}
	}
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if value == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, trimYAMLScalar(part))
		}
	}
	return out
}

func trimYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func validateBindingAmbiguity(bindings map[string]Binding) error {
	list := make([]Binding, 0, len(bindings))
	for _, b := range bindings {
		list = append(list, b)
	}
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if bindingsAmbiguous(list[i], list[j]) {
				return fmt.Errorf("bindings %q and %q can match the same request", list[i].Name, list[j].Name)
			}
		}
	}
	return nil
}

func bindingsAmbiguous(a, b Binding) bool {
	if !stringSlicesOverlap(a.Match.Schemes, b.Match.Schemes) ||
		!intSlicesOverlap(a.Match.Ports, b.Match.Ports) ||
		!stringSlicesOverlap(a.Match.Methods, b.Match.Methods) ||
		!pathPatternsOverlap(a.Match.Paths, b.Match.Paths) {
		return false
	}
	return hostSetsAmbiguousAtSamePrecedence(a.Match.Hosts, b.Match.Hosts)
}

func hostSetsAmbiguousAtSamePrecedence(aHosts, bHosts []string) bool {
	for _, a := range aHosts {
		for _, b := range bHosts {
			aWild := strings.HasPrefix(a, "*.")
			bWild := strings.HasPrefix(b, "*.")
			if aWild != bWild {
				continue
			}
			if !aWild && a == b {
				return true
			}
			if aWild && wildcardHostsOverlap(a, b) {
				return true
			}
		}
	}
	return false
}

func wildcardHostsOverlap(a, b string) bool {
	aSuffix := strings.TrimPrefix(a, "*.")
	bSuffix := strings.TrimPrefix(b, "*.")
	return aSuffix == bSuffix || strings.HasSuffix(aSuffix, "."+bSuffix) || strings.HasSuffix(bSuffix, "."+aSuffix)
}

func pathPatternsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if pathPatternOverlaps(x, y) {
				return true
			}
		}
	}
	return false
}

func pathPatternOverlaps(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasSuffix(a, "*") {
		if strings.HasPrefix(b, strings.TrimSuffix(a, "*")) {
			return true
		}
	}
	if strings.HasSuffix(b, "*") {
		if strings.HasPrefix(a, strings.TrimSuffix(b, "*")) {
			return true
		}
	}
	if strings.HasSuffix(a, "*") && strings.HasSuffix(b, "*") {
		pa := strings.TrimSuffix(a, "*")
		pb := strings.TrimSuffix(b, "*")
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	}
	return false
}

func stringSlicesOverlap(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}

func intSlicesOverlap(a, b []int) bool {
	set := make(map[int]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}

func canonicalHeaderName(name string) string {
	return http.CanonicalHeaderKey(name)
}

func dedupeStringsInPlace(values *[]string) {
	seen := make(map[string]struct{}, len(*values))
	out := (*values)[:0]
	for _, value := range *values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	*values = out
}

func dedupeIntsInPlace(values *[]int) {
	seen := make(map[int]struct{}, len(*values))
	out := (*values)[:0]
	for _, value := range *values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	*values = out
}

func ReadJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxCredentialVaultBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func WriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case strings.Contains(err.Error(), "expectedRevision"):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
