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

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type taskClientCreator func(ip string) taskClient

func newTaskStatusCollector(creator taskClientCreator, logger logr.Logger) taskStatusCollector {
	return &defaultTaskStatusCollector{creator: creator, logger: logger}
}

type taskStatusCollector interface {
	Collect(ctx context.Context, ipList []string) (map[string]*api.Task, error) /*ip<->task*/
}

// TODO maybe cache
type defaultTaskStatusCollector struct {
	creator taskClientCreator
	logger  logr.Logger
}

func (s *defaultTaskStatusCollector) Collect(ctx context.Context, ipList []string) (map[string]*api.Task, error) {
	semaphore := make(chan struct{}, len(ipList))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ret := make(map[string]*api.Task, len(ipList))
	var errs []error
	for idx := range ipList {
		ip := ipList[idx]
		semaphore <- struct{}{}
		wg.Add(1)
		go func(ip string) {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
			defer cancel()
			client := s.creator(ip)
			task, err := client.Get(ctx)
			if err != nil {
				s.logger.Error(err, "failed to GetTask", "ip", ip)
				mu.Lock()
				errs = append(errs, fmt.Errorf("get task status for pod IP %s: %w", ip, err))
				mu.Unlock()
			} else if task != nil {
				mu.Lock()
				ret[ip] = task
				mu.Unlock()
			} else {
				// Keep an explicit nil entry so callers can distinguish a
				// confirmed empty task list from a failed query.
				mu.Lock()
				ret[ip] = nil
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	verboseLog := s.logger.V(3)
	if verboseLog.Enabled() {
		verboseLog.Info("Collect task status", "result", utils.DumpJSON(ret))
	}
	return ret, errors.Join(errs...)
}
