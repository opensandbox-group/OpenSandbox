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
	"sync"

	"github.com/go-logr/logr"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

type taskClientCreator func(ip string) taskClient

func newTaskStatusCollector(creator taskClientCreator, logger logr.Logger) taskStatusCollector {
	return &defaultTaskStatusCollector{creator: creator, logger: logger}
}

// taskStatusCollector gathers task state from the pods' task-executor endpoints.
type taskStatusCollector interface {
	Collect(ctx context.Context, ipList []string) map[string]*api.Task /*ip<->task*/
}

type defaultTaskStatusCollector struct {
	creator taskClientCreator
	logger  logr.Logger
}

func (s *defaultTaskStatusCollector) Collect(ctx context.Context, ipList []string) map[string]*api.Task {
	semaphore := make(chan struct{}, len(ipList))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ret := make(map[string]*api.Task, len(ipList))
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
			} else if task != nil {
				mu.Lock()
				ret[ip] = task
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	s.logger.Info("Collect task status", "result", utils.DumpJSON(ret))
	return ret
}
