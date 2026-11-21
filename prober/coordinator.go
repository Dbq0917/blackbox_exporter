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

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ProbeCoordinator coalesces concurrent probes for the same module, target and
// hostname. Only the first caller runs the probe. Other callers wait for that
// probe to finish and receive its result, so a retry cannot start a second
// probe while the previous attempt is still in flight.
//
// The coordinator keeps no result after the probe completes. A later request
// always starts a new probe.
type ProbeCoordinator struct {
	mu       sync.Mutex
	inFlight map[probeKey]*probeRun
}

type probeKey struct {
	moduleName string
	target     string
	hostname   string
}

type probeRun struct {
	done   chan struct{}
	result probeResult
}

type probeResult struct {
	registry    *prometheus.Registry
	debugOutput string
}

// NewProbeCoordinator creates an empty probe coordinator.
func NewProbeCoordinator() *ProbeCoordinator {
	return &ProbeCoordinator{inFlight: make(map[probeKey]*probeRun)}
}

// do runs probe at most once across concurrent calls with the same key.
// Callers that arrive while a probe is running wait for it or for their own
// context to be canceled. A nil coordinator always runs probe.
func (c *ProbeCoordinator) do(ctx context.Context, key probeKey, probe func() probeResult) (*probeResult, error) {
	if c == nil {
		result := probe()
		return &result, nil
	}

	c.mu.Lock()
	if run, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-run.done:
			return &run.result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	run := &probeRun{done: make(chan struct{})}
	c.inFlight[key] = run
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		c.mu.Unlock()
		close(run.done)
	}()

	run.result = probe()
	return &run.result, nil
}
