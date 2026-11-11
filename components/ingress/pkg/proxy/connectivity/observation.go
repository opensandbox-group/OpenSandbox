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

package connectivity

import "time"

// Observation describes one TCP connection attempt as seen by the ingress.
type Observation struct {
	At       time.Time
	Protocol string
	// Target is the TCP dial address. Trackers normalize it to a network host.
	Target   string
	Result   Result
	Duration time.Duration
}

// Observer consumes TCP connection observations.
type Observer interface {
	Observe(Observation)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(Observation)

// Observe implements Observer.
func (f ObserverFunc) Observe(observation Observation) {
	f(observation)
}
