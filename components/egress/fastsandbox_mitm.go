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

// Fast Sandbox-profile shared mitmproxy assembly: ONE mitmdump in the fastlet Pod
// netns serving every sandbox (OSEP-0022 A1). Unlike the sidecar profile, the
// Pod-netns OUTPUT REDIRECT is deliberately NOT installed (it would intercept
// the Pod's own traffic); interception is the per-subject prerouting DNAT the
// fast-sandbox server installs (pkg/iptables.InstallMitmRedirects), keyed by sandbox
// source IP. The listener binds 0.0.0.0 so DNATed traffic landing on the
// gateway veth address reaches it (same reason the fast-sandbox DNS proxy binds
// :15353).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/internal/safego"
)

// startFastSandboxMitmproxyIfEnabled starts the shared mitmdump (transparent mode,
// all interfaces) on the pool-pinned CA and surfaces the CA fingerprint on the
// healthz gate. Returns nil when OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT is
// unset. A missing or unloadable pinned CA fails the MITM addon closed — the
// error is returned (the caller keeps egress serving) and the gate carries the
// explicit addon-down reason; mitmdump is never allowed to generate a CA.
func startFastSandboxMitmproxyIfEnabled(gate *mitmproxy.HealthGate) (*mitmTransparent, error) {
	if !constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent)) {
		return nil, nil
	}

	mpPort := constants.EnvIntOrDefault(constants.EnvMitmproxyPort, constants.DefaultMitmproxyPort)
	mpUID, mpGID, mpHome, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w (ensure this user exists in the image)", mitmproxy.RunAsUser, err)
	}
	confDir := filepath.Join(mpHome, ".mitmproxy")

	// Seed the pinned CA BEFORE the first launch: mitmdump loads a confdir CA
	// when present and only generates when missing — in this profile it must
	// never generate (OSEP-0022 MITM Trust Anchor).
	fp, err := mitmproxy.SeedPinnedCA(os.Getenv(constants.EnvMitmCaSeedDir), confDir, int(mpUID), int(mpGID))
	if err != nil {
		gate.MarkDown(fmt.Sprintf("pinned CA unavailable: %v", err))
		return nil, fmt.Errorf("pinned CA seed: %w", err)
	}
	gate.SetCAFingerprint(fp)

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
	log.Infof("fast-sandbox mitmproxy: shared transparent listener on 0.0.0.0:%d (interception via per-subject prerouting DNAT)", mpPort)

	// Startup consistency check (supersedes the fast-sandbox CA export, which
	// this profile drops — the guest trusts the template-baked certificate):
	// BOTH confdir CA files — the bundle mitmdump actually loads and the
	// public certificate copy — must match the pinned cert; a mismatch means
	// a CA was generated or swapped behind our back. Fail the addon closed.
	if err := mitmproxy.VerifyConfdirPinnedCA(confDir, fp); err != nil {
		gate.MarkDown(fmt.Sprintf("confdir CA mismatch: %v", err))
		mitmproxy.GracefulShutdown(running, 2*time.Second)
		return nil, fmt.Errorf("pinned CA consistency check: %w", err)
	}
	log.Infof("fast-sandbox mitmproxy: pinned CA loaded (fingerprint %s)", fp)
	return &mitmTransparent{
		running:    running,
		currentGen: initialGen,
		port:       mpPort,
		uid:        mpUID,
		cfg:        cfg,
		nextGen:    initialGen,
		restartCh:  restartCh,
		shutdownCh: shutdownCh,
		watchDone:  make(chan struct{}),
	}, nil
}

// fastSandboxActiveSocketPath is the shared unix socket for the subject-aware active
// vault API; the addon (mitmscripts/system.py) queries it per flow with the
// client's source IP. Same default location as the sidecar (per-Pod, one
// egress process).
func fastSandboxActiveSocketPath() string {
	return envOrDefault(constants.EnvCredentialProxySocket, constants.DefaultCredentialProxySocket)
}

// startFastSandboxActiveSocket starts the subject-aware active vault API on the
// shared unix socket: one socket, dispatch inside (clientIp -> subject ->
// vault snapshot). Cleaned up on ctx cancellation.
func startFastSandboxActiveSocket(ctx context.Context, srv *fastSandboxPolicyServer) {
	_, mitmGID, _, err := mitmproxy.LookupUser(mitmproxy.RunAsUser)
	if err != nil {
		log.Fatalf("lookup credential proxy user %q: %v (ensure this user exists in the image)", mitmproxy.RunAsUser, err)
	}
	socketPath := fastSandboxActiveSocketPath()
	activeSrv, cleanup, err := credentialvault.StartActiveSocketServerRequestAware(srv.handleCredentialVaultActive, socketPath, int(mitmGID))
	if err != nil {
		log.Fatalf("credential vault active socket: %v", err)
	}
	log.Infof("fast-sandbox credential vault active API listening on unix socket %s", socketPath)
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
