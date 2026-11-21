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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProbeCoordinatorCoalescesInFlightProbe(t *testing.T) {
	coordinator := NewProbeCoordinator()
	key := probeKey{moduleName: "http_2xx", target: "target"}

	existing := &probeRun{
		done:   make(chan struct{}),
		result: probeResult{debugOutput: "shared result"},
	}
	coordinator.inFlight[key] = existing
	close(existing.done)

	var calls atomic.Int32
	probe := func() probeResult {
		calls.Add(1)
		return probeResult{}
	}

	result, err := coordinator.do(context.Background(), key, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("probe ran %d times, want 0", got)
	}
	if result != &existing.result {
		t.Fatal("retry did not receive the in-flight probe result")
	}
}

func TestProbeCoordinatorWaiterHonorsContext(t *testing.T) {
	coordinator := NewProbeCoordinator()
	key := probeKey{moduleName: "http_2xx", target: "target"}

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	probe := func() probeResult {
		calls.Add(1)
		close(started)
		<-release
		return probeResult{}
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = coordinator.do(context.Background(), key, probe)
	})
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.do(ctx, key, probe); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter returned %v, want %v", err, context.Canceled)
	}

	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times, want 1", got)
	}
}

func TestProbeCoordinatorRunsNewProbeAfterCompletion(t *testing.T) {
	coordinator := NewProbeCoordinator()
	key := probeKey{moduleName: "http_2xx", target: "target"}

	var calls atomic.Int32
	probe := func() probeResult {
		calls.Add(1)
		return probeResult{}
	}

	if _, err := coordinator.do(context.Background(), key, probe); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.do(context.Background(), key, probe); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe ran %d times, want 2", got)
	}
}

func TestProbeCoordinatorSeparatesKeys(t *testing.T) {
	coordinator := NewProbeCoordinator()

	var calls atomic.Int32
	probe := func() probeResult {
		calls.Add(1)
		return probeResult{}
	}

	keys := []probeKey{
		{moduleName: "http_2xx", target: "target"},
		{moduleName: "http_2xx", target: "target", hostname: "example.com"},
		{moduleName: "tcp_connect", target: "target"},
	}
	for _, key := range keys {
		if _, err := coordinator.do(context.Background(), key, probe); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != int32(len(keys)) {
		t.Fatalf("probe ran %d times, want %d", got, len(keys))
	}
}
