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

package idlepolicy

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"knative.dev/pkg/injection"

	"github.com/alibaba/opensandbox/ingress/pkg/telemetry"
)

// SandboxGVR is the fast-sandbox Sandbox CR the annotation lives on.
var SandboxGVR = schema.GroupVersionResource{Group: "sandbox.fast.io", Version: "v1alpha2", Resource: "sandboxes"}

// DefaultCacheTTL bounds one CR read.
const DefaultCacheTTL = 30 * time.Second

// failureBackoff rate-limits retries after a failed read: an outage or RBAC
// misconfiguration must not turn into one GET per request.
const failureBackoff = 2 * time.Second

// Gate outcomes reported by ingress.idlep.policy.reads.
const (
	outcomeAllowed = "allowed"
	outcomeDenied  = "denied"
	outcomeError   = "error"
)

// PolicySource answers the wake gate for one sandbox; must fail safe.
type PolicySource interface {
	WakeAllowed(ctx context.Context, namespace, sandboxID string) bool
}

// CRPolicySource reads the idle-policy annotation via a Kubernetes API GET
// of the Sandbox CR, with a TTL cache and per-key read collapsing.
type CRPolicySource struct {
	client dynamic.Interface
	gvr    schema.GroupVersionResource
	ttl    time.Duration
	now    func() time.Time

	mu         sync.Mutex
	cache      map[cacheKey]cacheEntry
	failures   map[cacheKey]time.Time
	inflight   map[cacheKey]*inflightRead
	failureTTL time.Duration
}

type cacheKey struct {
	namespace string
	sandboxID string
}

type cacheEntry struct {
	allowed bool
	at      time.Time
}

// inflightRead collapses concurrent cold-cache reads for one key.
type inflightRead struct {
	done    chan struct{}
	allowed bool
	ok      bool
}

// fetchTimeout bounds one API read; a hung apiserver must not hang the wake
// path past the park budget.
const fetchTimeout = 2 * time.Second

func NewCRPolicySource(ctx context.Context, client dynamic.Interface, ttl time.Duration) *CRPolicySource {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	s := &CRPolicySource{
		client:     client,
		gvr:        SandboxGVR,
		ttl:        ttl,
		now:        time.Now,
		cache:      make(map[cacheKey]cacheEntry),
		failures:   make(map[cacheKey]time.Time),
		inflight:   make(map[cacheKey]*inflightRead),
		failureTTL: failureBackoff,
	}
	go s.runCacheJanitor(ctx)
	return s
}

// NewInClusterCRPolicySource builds a CRPolicySource from the
// environment's Kubernetes credentials.
func NewInClusterCRPolicySource(ctx context.Context) (*CRPolicySource, error) {
	cfg := injection.ParseAndGetRESTConfigOrDie()
	cfg.UserAgent = "opensandbox-ingress-idlepolicy"
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewCRPolicySource(ctx, client, DefaultCacheTTL), nil
}

// WakeAllowed reports whether the sandbox's policy permits wake-on-access;
// every failure resolves to false.
func (s *CRPolicySource) WakeAllowed(ctx context.Context, namespace, sandboxID string) bool {
	allowed, outcome := s.decide(ctx, namespace, sandboxID)
	telemetry.RecordPolicyRead(outcome)
	return allowed
}

func (s *CRPolicySource) decide(ctx context.Context, namespace, sandboxID string) (bool, string) {
	key := cacheKey{namespace: namespace, sandboxID: sandboxID}
	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && s.now().Sub(entry.at) < s.ttl {
		s.mu.Unlock()
		return entry.allowed, policyOutcome(entry.allowed)
	}
	// Failure backoff: rate-limit retries without a sticky denial.
	if at, ok := s.failures[key]; ok && s.now().Sub(at) < s.failureTTL {
		s.mu.Unlock()
		return false, outcomeError
	}
	// Collapse the cold-cache stampede: one read per key, joiners wait.
	if call, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-call.done:
			if !call.ok {
				return false, outcomeError
			}
			return call.allowed, policyOutcome(call.allowed)
		case <-ctx.Done():
			return false, outcomeError
		}
	}
	call := &inflightRead{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	allowed, ok := s.fetch(ctx, namespace, sandboxID)

	// Cache successes only; failures get a short backoff window instead of
	// a sticky denial for the full TTL.
	s.mu.Lock()
	if ok {
		s.cache[key] = cacheEntry{allowed: allowed, at: s.now()}
	} else {
		s.failures[key] = s.now()
	}
	delete(s.inflight, key)
	s.mu.Unlock()
	call.allowed, call.ok = allowed, ok
	close(call.done)
	if !ok {
		return false, outcomeError
	}
	return allowed, policyOutcome(allowed)
}

func policyOutcome(allowed bool) string {
	if allowed {
		return outcomeAllowed
	}
	return outcomeDenied
}

func (s *CRPolicySource) fetch(ctx context.Context, namespace, sandboxID string) (allowed, ok bool) {
	// Detach from the leader's request context: the leader disconnecting
	// must not abort the read its joiners are waiting on.
	// resourceVersion "0" = accepted-stale; the gate tolerates staleness by
	// design.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	cr, err := s.client.Resource(s.gvr).Namespace(namespace).Get(ctx, sandboxID, metav1.GetOptions{ResourceVersion: "0"})
	if err != nil {
		return false, false
	}
	return WakeAllowedOf(cr.GetAnnotations()[AnnotationKey]), true
}

func (s *CRPolicySource) runCacheJanitor(ctx context.Context) {
	ticker := time.NewTicker(s.ttl * 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := s.now().Add(-s.ttl)
			failCutoff := s.now().Add(-s.failureTTL)
			s.mu.Lock()
			for key, entry := range s.cache {
				if entry.at.Before(cutoff) {
					delete(s.cache, key)
				}
			}
			for key, at := range s.failures {
				if at.Before(failCutoff) {
					delete(s.failures, key)
				}
			}
			s.mu.Unlock()
		}
	}
}
