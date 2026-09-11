// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"context"
	"fmt"
)

// CompositeProvider routes legacy and Fast Sandbox targets to their configured providers.
type CompositeProvider struct {
	legacy Provider
	fsb    Provider
}

// NewCompositeProvider creates a provider that serves both route kinds.
func NewCompositeProvider(legacy Provider, fsb Provider) *CompositeProvider {
	return &CompositeProvider{legacy: legacy, fsb: fsb}
}

func (p *CompositeProvider) Start(ctx context.Context) error {
	if err := p.legacy.Start(ctx); err != nil {
		return fmt.Errorf("start legacy provider: %w", err)
	}
	if err := p.fsb.Start(ctx); err != nil {
		return fmt.Errorf("start Fast Sandbox provider: %w", err)
	}
	return nil
}

func (p *CompositeProvider) ResolveEndpoint(ctx context.Context, target EndpointTarget) (*EndpointInfo, error) {
	switch target.RouteKind {
	case RouteKindLegacy:
		return p.legacy.ResolveEndpoint(ctx, target)
	case RouteKindFastSandbox:
		return p.fsb.ResolveEndpoint(ctx, target)
	default:
		return nil, fmt.Errorf("%w: route kind %d", ErrTargetUnsupported, target.RouteKind)
	}
}

func (p *CompositeProvider) Invalidate(target EndpointTarget) {
	if target.RouteKind != RouteKindFastSandbox {
		return
	}
	if invalidator, ok := p.fsb.(EndpointInvalidator); ok {
		invalidator.Invalidate(target)
	}
}

var _ Provider = (*CompositeProvider)(nil)
var _ EndpointInvalidator = (*CompositeProvider)(nil)
