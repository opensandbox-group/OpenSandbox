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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type namedHook struct {
	name string
	run  func(context.Context) error
}

func (n namedHook) Name() string                  { return n.name }
func (n namedHook) Run(ctx context.Context) error { return n.run(ctx) }

func TestRunPost_empty(t *testing.T) {
	resetHooksForTest()
	t.Cleanup(resetHooksForTest)
	require.NoError(t, RunPost(context.Background()))
}

func TestRunPost_orderAndError(t *testing.T) {
	resetHooksForTest()
	t.Cleanup(resetHooksForTest)

	var n int
	Register(namedHook{
		name: "a",
		run: func(context.Context) error {
			n++
			require.Equal(t, 1, n)
			return nil
		},
	})
	Register(namedHook{
		name: "b",
		run: func(context.Context) error {
			n++
			require.Equal(t, 2, n)
			return errors.New("boom")
		},
	})
	Register(namedHook{
		name: "c",
		run: func(context.Context) error {
			n++
			return nil
		},
	})

	err := RunPost(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "b")
	require.Contains(t, err.Error(), "boom")
	require.Equal(t, 2, n)
}
