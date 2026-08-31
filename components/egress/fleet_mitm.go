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

// Fleet-profile shared mitmproxy assembly: ONE mitmdump in the fastlet Pod
// netns serving every sandbox (OSEP-0022 A1). Unlike the sidecar profile, the
// Pod-netns OUTPUT REDIRECT is deliberately NOT installed (it would intercept
// the Pod's own traffic); interception is the per-subject prerouting DNAT the
// fleet server installs (pkg/iptables.InstallMitmRedirects), keyed by sandbox
// source IP. The listener binds 0.0.0.0 so DNATed traffic landing on the
// gateway veth address reaches it (same reason the fleet DNS proxy binds
// :15353).
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/internal/safego"
)

// startFleetMitmproxyIfEnabled starts the shared mitmdump (transparent mode,
// all interfaces), waits for the listener, and exports the CA into the fleet
// subdir that fastlet bind-mounts into every sandbox. Returns nil when
// OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT is unset.
func startFleetMitmproxyIfEnabled() (*mitmTransparent, error) {
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return nil, nil
	}

	mpPort := constants.EnvIntOrDefault(constants.EnvMitmproxyPort, constants.DefaultMitmproxyPort)
	mpUID, _, mpHome, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w (ensure this user exists in the image)", mitmproxy.RunAsUser, err)
	}

	cfg := mitmproxy.Config{
		ListenPort:  mpPort,
		ListenHost:  "0.0.0.0",
		UserName:    mitmproxy.RunAsUser,
		ScriptPaths: parseScriptPaths(os.Getenv(constants.EnvMitmproxyScript)),
	}
	restartCh := make(chan exitEvent, 64)
	shutdownCh := make(chan struct{})
	const initialGen uint64 = 1
	running, err := launchTagged(cfg, restartCh, shutdownCh, initialGen)
	if err != nil {
		return nil, fmt.Errorf("start mitmdump: %w", err)
	}

	// 0.0.0.0 covers loopback, so the loopback wait check still works.
	waitAddr := fmt.Sprintf("127.0.0.1:%d", mpPort)
	if err := mitmproxy.WaitListenPort(waitAddr, 15*time.Second); err != nil {
		return nil, fmt.Errorf("wait listen %s: %w", waitAddr, err)
	}
	log.Infof("fleet mitmproxy: shared transparent listener on 0.0.0.0:%d (interception via per-subject prerouting DNAT)", mpPort)

	// No Pod-netns OUTPUT REDIRECT (would intercept the Pod's own traffic);
	// the fleet server installs the per-subject prerouting DNAT instead.
	if err := mitmproxy.SyncRootCAFleet("", mpHome); err != nil {
		return nil, fmt.Errorf("mitm CA export: %w", err)
	}
	return &mitmTransparent{
		running:    running,
		currentGen: initialGen,
		port:       mpPort,
		uid:        mpUID,
		cfg:        cfg,
		nextGen:    initialGen,
		restartCh:  restartCh,
		shutdownCh: shutdownCh,
	}, nil
}

// fleetActiveSocketPath is the shared unix socket for the subject-aware active
// vault API; the addon (mitmscripts/system.py) queries it per flow with the
// client's source IP. Same default location as the sidecar (per-Pod, one
// egress process).
func fleetActiveSocketPath() string {
	return envOrDefault(constants.EnvCredentialProxySocket, constants.DefaultCredentialProxySocket)
}

// startFleetActiveSocket starts the subject-aware active vault API on the
// shared unix socket: one socket, dispatch inside (clientIp -> subject ->
// vault snapshot). Cleaned up on ctx cancellation.
func startFleetActiveSocket(ctx context.Context, srv *fleetPolicyServer) {
	_, mitmGID, _, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	if err != nil {
		log.Fatalf("lookup credential proxy user %q: %v (ensure this user exists in the image)", mitmproxy.RunAsUser, err)
	}
	socketPath := fleetActiveSocketPath()
	activeSrv, cleanup, err := credentialvault.StartActiveSocketServerRequestAware(srv.handleCredentialVaultActive, socketPath, int(mitmGID))
	if err != nil {
		log.Fatalf("credential vault active socket: %v", err)
	}
	log.Infof("fleet credential vault active API listening on unix socket %s", socketPath)
	_ = activeSrv
	safego.Go(func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cleanup(shutdownCtx); err != nil {
			log.Errorf("credential vault active socket shutdown error: %v", err)
		}
	})
}
