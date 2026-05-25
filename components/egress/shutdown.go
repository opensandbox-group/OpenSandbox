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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/iptables"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
)

const (
	defaultPolicyShutdownTimeout = 5 * time.Second
	defaultNftTeardownTimeout    = 5 * time.Second
	defaultMitmShutdownTimeout   = 5 * time.Second
)

func waitForShutdown(ctx context.Context, proxy *dnsproxy.Proxy, policySrv *http.Server, exemptDst []netip.Addr, applier nftApplier, mitm *mitmTransparent) {
	<-ctx.Done()
	log.Infof("received shutdown signal; beginning graceful shutdown")

	policyShutdownCtx, policyCancel := context.WithTimeout(context.Background(), defaultPolicyShutdownTimeout)
	defer policyCancel()

	if policySrv != nil {
		if err := policySrv.Shutdown(policyShutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("policy server shutdown error: %v", err)
		}
	}

	proxy.SetOnResolved(nil)

	if err := proxy.Shutdown(); err != nil {
		log.Errorf("dns proxy shutdown error: %v", err)
	}

	if mitm != nil {
		iptables.RemoveTransparentHTTP(mitm.port, mitm.uid)
		mitmproxy.GracefulShutdown(mitm.getRunning(), defaultMitmShutdownTimeout)
	}
	iptables.RemoveRedirect(15353, exemptDst)

	if applier != nil {
		nftCtx, nftCancel := context.WithTimeout(context.Background(), defaultNftTeardownTimeout)
		defer nftCancel()
		if err := applier.RemoveEnforcement(nftCtx); err != nil {
			log.Errorf("nftables teardown error: %v", err)
		}
	}

	log.Infof("egress shutdown complete")
	_ = os.Stderr.Sync()
}
