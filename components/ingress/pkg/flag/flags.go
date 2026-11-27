// Copyright 2025 The OpenSandbox Authors
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

package flag

import (
	"time"

	"knative.dev/pkg/environment"
)

var (
	LogLevel     string
	Port         int
	ProviderType string
	Mode         string
	ClientConfig environment.ClientConfig
)

var (
	HTTPMaxIdleConns        int
	HTTPMaxIdleConnsPerHost int
	HTTPIdleConnTimeout     time.Duration
)

var (
	RenewIntentEnabled        bool
	RenewIntentRedisDSN       string
	RenewIntentQueueKey       string
	RenewIntentQueueMaxLen    int
	RenewIntentMinIntervalSec int
)

var (
	ActivityEnabled     bool
	ActivityRedisDSN    string
	ActivityMinInterval time.Duration
	// Must match the server TTL and cover every accepted idle threshold.
	ActivityTTLSeconds int

	WakeEnabled           bool
	WakeParkBudget        time.Duration
	WakeParkMax           int
	WakeRestoreMax        int
	WakeMaxFlightLifetime time.Duration
	WakeRetryInterval     time.Duration
	WakeRetryFactor       float64
	WakeRetryJitter       float64
)

var (
	SecureAccessKeys string

	FastPathEndpoint          string
	FastPathAuthEnabled       bool
	FastPathTLSCAFile         string
	FastPathTLSServerName     string
	FastSandboxNamespace      string
	FastPathAccessMode        string
	FastPathWaitTimeoutMillis int
)

var (
	NetworkReadinessShadowWindow               time.Duration
	NetworkReadinessShadowMaxTargets           int
	NetworkReadinessShadowMinAttempts          uint64
	NetworkReadinessShadowMinTargets           int
	NetworkReadinessShadowMinSignalTargets     int
	NetworkReadinessShadowDegradedFailureRatio float64
)
