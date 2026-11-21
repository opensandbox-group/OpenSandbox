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

package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const fastPathAudience = "fast-sandbox-fastpath"

type FastPathAuthConfig struct {
	Kubernetes     *rest.Config
	Namespace      string
	ServiceAccount string
	CAFile         string
	ServerName     string
}

type fastPathTokenSource struct {
	create                  func(context.Context) (*authenticationv1.TokenRequest, error)
	mu                      sync.Mutex
	token                   string
	expires, refresh, retry time.Time
	backoff                 time.Duration
	flight                  chan struct{}
	now                     func() time.Time
}

func newFastPathTokenSource(config FastPathAuthConfig) (*fastPathTokenSource, error) {
	if config.Kubernetes == nil || config.Namespace == "" || config.ServiceAccount == "" {
		return nil, fmt.Errorf("FastPath requires Kubernetes credentials and a caller ServiceAccount namespace/name")
	}
	if config.Kubernetes.Insecure || !strings.HasPrefix(config.Kubernetes.Host, "https://") {
		return nil, fmt.Errorf("FastPath TokenRequest requires verified Kubernetes API TLS")
	}
	client, err := corev1.NewForConfig(config.Kubernetes)
	if err != nil {
		return nil, err
	}
	return &fastPathTokenSource{now: time.Now, create: func(ctx context.Context) (*authenticationv1.TokenRequest, error) {
		duration := int64(3600)
		return client.ServiceAccounts(config.Namespace).CreateToken(ctx, config.ServiceAccount, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: []string{fastPathAudience}, ExpirationSeconds: &duration},
		}, metav1.CreateOptions{})
	}}, nil
}

func (s *fastPathTokenSource) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	for {
		s.mu.Lock()
		now := s.now()
		valid := s.token != "" && now.Before(s.expires.Add(-5*time.Second))
		if valid && (now.Before(s.refresh) || now.Before(s.retry)) {
			token := s.token
			s.mu.Unlock()
			return map[string]string{"authorization": "Bearer " + token}, nil
		}
		if now.Before(s.retry) {
			s.mu.Unlock()
			return nil, fmt.Errorf("FastPath TokenRequest unavailable; check Kubernetes credentials and caller RBAC")
		}
		if s.flight == nil {
			s.flight = make(chan struct{})
			go s.mint(s.flight)
		}
		flight := s.flight
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight:
		}
	}
}

func (s *fastPathTokenSource) mint(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	issued, err := s.create(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err == nil && issued != nil && issued.Status.Token != "" && issued.Status.ExpirationTimestamp.Time.After(now.Add(5*time.Second)) {
		s.token, s.expires = issued.Status.Token, issued.Status.ExpirationTimestamp.Time
		margin := min(20*time.Minute, s.expires.Sub(now)/2)
		s.refresh = s.expires.Add(-time.Duration(float64(margin) * (0.9 + rand.Float64()*0.1)))
		s.retry, s.backoff = time.Time{}, 0
	} else {
		s.backoff = min(30*time.Second, max(time.Second, s.backoff*2))
		s.retry = now.Add(s.backoff)
	}
	s.flight = nil
	close(done)
}

func (*fastPathTokenSource) RequireTransportSecurity() bool { return true }

type fastPathTLS struct {
	credentials.TransportCredentials
	caFile, serverName string
}

func newFastPathTLS(caFile, serverName string) (*fastPathTLS, error) {
	c := &fastPathTLS{caFile: caFile, serverName: serverName}
	transport, err := c.load()
	if err != nil {
		return nil, err
	}
	c.TransportCredentials = transport
	return c, nil
}

func (c *fastPathTLS) load() (credentials.TransportCredentials, error) {
	var roots *x509.CertPool
	if c.caFile != "" {
		pem, err := os.ReadFile(c.caFile)
		if err != nil {
			return nil, fmt.Errorf("read FastPath serving CA: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("FastPath serving CA contains no certificates")
		}
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: c.serverName}), nil
}

func (c *fastPathTLS) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	transport, err := c.load()
	if err != nil {
		return nil, nil, err
	}
	return transport.ClientHandshake(ctx, authority, conn)
}

func (c *fastPathTLS) Clone() credentials.TransportCredentials {
	return &fastPathTLS{TransportCredentials: c.TransportCredentials.Clone(), caFile: c.caFile, serverName: c.serverName}
}

func (c *fastPathTLS) OverrideServerName(name string) error {
	c.serverName = name
	return c.TransportCredentials.OverrideServerName(name)
}
