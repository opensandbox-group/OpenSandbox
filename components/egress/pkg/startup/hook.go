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

package startup

import (
	"context"
	"fmt"
	"sync"
)

type Hook interface {
	Name() string
	Run(ctx context.Context) error
}

var (
	mu    sync.Mutex
	hooks []Hook
)

func Register(h Hook) {
	if h == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	hooks = append(hooks, h)
}

func RegisterFunc(name string, fn func(ctx context.Context) error) {
	if fn == nil {
		return
	}
	Register(funcHook{name: name, fn: fn})
}

type funcHook struct {
	name string
	fn   func(context.Context) error
}

func (f funcHook) Name() string { return f.name }

func (f funcHook) Run(ctx context.Context) error { return f.fn(ctx) }

func RunPost(ctx context.Context) error {
	mu.Lock()
	list := append([]Hook(nil), hooks...)
	mu.Unlock()
	for _, h := range list {
		if err := h.Run(ctx); err != nil {
			return fmt.Errorf("startup hook %q (post): %w", h.Name(), err)
		}
	}
	return nil
}

func resetHooksForTest() {
	mu.Lock()
	defer mu.Unlock()
	hooks = nil
}
