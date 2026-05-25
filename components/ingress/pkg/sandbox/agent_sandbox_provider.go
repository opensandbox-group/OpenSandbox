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

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	agentSandboxGroup    = "agents.x-k8s.io"
	agentSandboxVersion  = "v1alpha1"
	agentSandboxResource = "sandboxes"

	agentSandboxConditionReady = "Ready"
	agentSandboxNamePrefix     = "sandbox"
)

var (
	dns1035InvalidChars     = regexp.MustCompile(`[^a-z0-9-]+`)
	dns1035DuplicateHyphens = regexp.MustCompile(`-+`)
)

type AgentSandboxProvider struct {
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	informer        cache.SharedIndexInformer
	gvr             schema.GroupVersionResource
}

func NewAgentSandboxProvider(config *rest.Config, resyncPeriod time.Duration) *AgentSandboxProvider {
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("failed to create dynamic client: %v", err))
	}

	return newAgentSandboxProviderWithClient(dyn, resyncPeriod)
}

func newAgentSandboxProviderWithClient(dyn dynamic.Interface, resyncPeriod time.Duration) *AgentSandboxProvider {
	gvr := schema.GroupVersionResource{
		Group:    agentSandboxGroup,
		Version:  agentSandboxVersion,
		Resource: agentSandboxResource,
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn,
		resyncPeriod,
		metav1.NamespaceAll,
		nil, // no extra list options
	)

	informer := factory.ForResource(gvr).Informer()
	if err := informer.AddIndexers(cache.Indexers{
		sandboxNameIndex: func(obj any) ([]string, error) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return []string{}, nil
			}
			return []string{u.GetName()}, nil
		},
	}); err != nil {
		panic(fmt.Sprintf("failed to add AgentSandbox indexer: %v", err))
	}

	return &AgentSandboxProvider{
		informerFactory: factory,
		informer:        informer,
		gvr:             gvr,
	}
}

func agentSandboxResourceName(sandboxId string) string {
	return toDNS1035Label(sandboxId, agentSandboxNamePrefix)
}

func toDNS1035Label(value, prefix string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = dns1035InvalidChars.ReplaceAllString(normalized, "-")
	normalized = dns1035DuplicateHyphens.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")

	hash := sha256.Sum256([]byte(value))
	suffix := hex.EncodeToString(hash[:])[:8]

	if normalized == "" {
		normalized = prefix + "-" + suffix
	} else if !startsWithLetter(normalized) {
		normalized = prefix + "-" + normalized
	}

	if len(normalized) > validation.DNS1035LabelMaxLength {
		maxBase := validation.DNS1035LabelMaxLength - len(suffix) - 1
		base := normalized
		if len(base) > maxBase {
			base = base[:maxBase]
		}
		base = strings.Trim(base, "-")
		if !startsWithLetter(base) {
			base = prefix
		}
		normalized = base + "-" + suffix
	}

	return strings.Trim(normalized, "-")
}

func startsWithLetter(value string) bool {
	if value == "" {
		return false
	}
	first := value[0]
	return first >= 'a' && first <= 'z'
}

func legacyAgentSandboxName(sandboxId string) string {
	legacyPrefix := agentSandboxNamePrefix + "-"
	if strings.HasPrefix(sandboxId, legacyPrefix) {
		return sandboxId
	}
	return legacyPrefix + sandboxId
}

func resourceNameCandidates(sandboxId string) []string {
	candidates := []string{}
	primary := agentSandboxResourceName(sandboxId)
	candidates = append(candidates, primary)
	if sandboxId != primary {
		candidates = append(candidates, sandboxId)
	}
	legacy := legacyAgentSandboxName(sandboxId)
	if legacy != primary && legacy != sandboxId {
		candidates = append(candidates, legacy)
	}
	return candidates
}

func (a *AgentSandboxProvider) lookupAgentSandbox(sandboxId string) (*unstructured.Unstructured, error) {
	candidates := resourceNameCandidates(sandboxId)
	for _, name := range candidates {
		indexed, err := a.informer.GetIndexer().ByIndex(sandboxNameIndex, name)
		if err != nil {
			return nil, fmt.Errorf("failed to query AgentSandbox index for %q: %w", name, err)
		}
		matches := make([]string, 0, len(indexed))
		var matchObj *unstructured.Unstructured
		for _, item := range indexed {
			u, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			matches = append(matches, fmt.Sprintf("%s/%s", u.GetNamespace(), u.GetName()))
			if matchObj == nil {
				matchObj = u
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("ambiguous sandbox id %q found in multiple namespaces: %v", sandboxId, matches)
		}
		if len(matches) == 1 {
			return matchObj, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxId)
}

func (a *AgentSandboxProvider) GetEndpoint(sandboxId string) (*EndpointInfo, error) {
	u, err := a.lookupAgentSandbox(sandboxId)
	if err != nil {
		return nil, err
	}
	endpoint, err := a.resolveEndpointFromSandbox(sandboxId, u)
	if err != nil {
		return nil, err
	}
	accessToken := ""
	ann := u.GetAnnotations()
	if ann != nil {
		accessToken = strings.TrimSpace(ann[AnnotationAccessToken])
	}
	return &EndpointInfo{
		Endpoint:          endpoint,
		SecureAccessToken: accessToken,
	}, nil
}

func (a *AgentSandboxProvider) resolveEndpointFromSandbox(sandboxId string, u *unstructured.Unstructured) (string, error) {
	status, ok := u.Object["status"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%w: sandbox %s missing status", ErrSandboxNotReady, sandboxId)
	}

	// Check ready condition first; must be Ready=True to proceed.
	if ready, reason, message := a.checkSandboxReadyCondition(status); !ready {
		return "", fmt.Errorf("%w: sandbox %s not ready (%s: %s)", ErrSandboxNotReady, sandboxId, reason, message)
	}

	serviceFQDN, _ := status["serviceFQDN"].(string)
	if serviceFQDN == "" {
		return "", fmt.Errorf("%w: sandbox %s has no serviceFQDN", ErrSandboxNotReady, sandboxId)
	}

	return serviceFQDN, nil
}

func (a *AgentSandboxProvider) Start(ctx context.Context) error {
	a.informerFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), a.informer.HasSynced) {
		return errors.New("failed to sync AgentSandbox informer cache")
	}

	return nil
}

// checkSandboxReadyCondition inspects status.conditions for Ready=True.
// Returns (isReady, reason, message).
//
// https://github.com/kubernetes-sigs/agent-sandbox/blob/main/controllers/sandbox_controller.go#L195
func (a *AgentSandboxProvider) checkSandboxReadyCondition(status map[string]any) (bool, string, string) {
	conds, ok := status["conditions"].([]any)
	if !ok {
		return false, "NoConditions", "no sandbox conditions reported"
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != agentSandboxConditionReady {
			continue
		}
		if s, _ := m["status"].(string); s == string(metav1.ConditionTrue) {
			return true, agentSandboxConditionReady, ""
		}
		reason, _ := m["reason"].(string)
		message, _ := m["message"].(string)
		if reason == "" {
			reason = "DependenciesNotReady"
		}
		if message == "" {
			message = "Ready condition is not True"
		}
		return false, reason, message
	}

	return false, "ReadyConditionMissing", "ready condition missing"
}

var _ Provider = (*AgentSandboxProvider)(nil)
