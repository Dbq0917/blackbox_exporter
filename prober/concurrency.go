// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

// ProbeConcurrencyLimit bounds the number of probes running at the same time.
// It never blocks a caller: when the limit is reached TryAcquire reports
// false, so healthy targets behind a slow, unreachable one do not queue and
// wait behind it. A zero limit disables the cap.
type ProbeConcurrencyLimit struct {
	sem chan struct{}
}

// NewProbeConcurrencyLimit creates a limiter allowing at most maxProbes
// concurrent probes. A value of zero disables limiting.
func NewProbeConcurrencyLimit(maxProbes int) *ProbeConcurrencyLimit {
	var sem chan struct{}
	if maxProbes > 0 {
		sem = make(chan struct{}, maxProbes)
	}
	return &ProbeConcurrencyLimit{sem: sem}
}

// TryAcquire reserves a probe slot. It returns a release function and true
// when a slot was available. When the limiter is disabled or already full it
// returns nil and false without blocking.
func (l *ProbeConcurrencyLimit) TryAcquire() (func(), bool) {
	if l == nil || l.sem == nil {
		return func() {}, true
	}
	select {
	case l.sem <- struct{}{}:
		return l.release, true
	default:
		return nil, false
	}
}

func (l *ProbeConcurrencyLimit) release() {
	<-l.sem
}
