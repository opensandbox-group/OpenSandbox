// Copyright 2025 Alibaba Group Holding Ltd.
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

package safego

import (
	"context"
	"net/http"
	"runtime"

	"github.com/alibaba/opensandbox/internal/logger"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
)

func InitPanicLogger(_ context.Context, log logger.Logger) {
	runtimeutil.PanicHandlers = []func(context.Context, any){
		func(_ context.Context, r any) {
			if r == http.ErrAbortHandler { // nolint:errorlint
				return
			}

			const size = 64 << 10
			stacktrace := make([]byte, size)
			stacktrace = stacktrace[:runtime.Stack(stacktrace, false)]
			if _, ok := r.(string); ok {
				log.Errorf("Observed a panic: %s\n%s", r, stacktrace)
			} else {
				log.Errorf("Observed a panic: %#v (%v)\n%s", r, r, stacktrace)
			}
		},
	}
}

func init() {
	runtimeutil.ReallyCrash = false
}

func Go(f func()) {
	go func() {
		defer runtimeutil.HandleCrash()

		f()
	}()
}
