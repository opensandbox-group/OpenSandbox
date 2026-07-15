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

package renewintent

import (
	"context"
	"fmt"
	"testing"

	"github.com/alibaba/opensandbox/internal/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type nopLogger struct{}

func (nopLogger) Debugf(string, ...any)                {}
func (nopLogger) Infof(string, ...any)                 {}
func (nopLogger) Warnf(string, ...any)                 {}
func (nopLogger) Errorf(string, ...any)                {}
func (n nopLogger) With(...logger.Field) logger.Logger { return n }
func (n nopLogger) Named(string) logger.Logger         { return n }
func (nopLogger) Sync() error                          { return nil }

// Benchmarks use miniredis (in-memory Redis) so timing excludes real network I/O.

func BenchmarkRedisPublisher_PublishIntent(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := RedisPublisherConfig{
		QueueKey:    "opensandbox:renew:intent",
		QueueMaxLen: 0,
		MinInterval: 0,
		Logger:      nopLogger{},
	}
	p := NewRedisPublisher(ctx, client, cfg)

	sandboxID := "bench-sandbox"
	port := 8080
	requestURI := "/api/health"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.PublishIntent(sandboxID, port, requestURI)
	}
}

func BenchmarkRedisPublisher_PublishIntent_Throttled(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := RedisPublisherConfig{
		QueueKey:    "opensandbox:renew:intent",
		QueueMaxLen: 0,
		MinInterval: 1 << 30, // large so throttle skips most
		Logger:      nopLogger{},
	}
	p := NewRedisPublisher(ctx, client, cfg)

	sandboxID := "bench-sandbox"
	port := 8080
	requestURI := "/api/health"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.PublishIntent(sandboxID, port, requestURI)
	}
}

func BenchmarkRedisPublisher_PublishIntent_ManySandboxes(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatal(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := RedisPublisherConfig{
		QueueKey:    "opensandbox:renew:intent",
		QueueMaxLen: 0,
		MinInterval: 0,
		Logger:      nopLogger{},
	}
	p := NewRedisPublisher(ctx, client, cfg)

	port := 8080
	requestURI := "/api/health"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sandboxID := fmt.Sprintf("sandbox-%d", i%1000)
		p.PublishIntent(sandboxID, port, requestURI)
	}
}
