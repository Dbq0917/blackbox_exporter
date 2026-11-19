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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

func TestProbeConcurrencyLimitAllowsUpToCapacity(t *testing.T) {
	limiter := NewProbeConcurrencyLimit(3)

	var releases []func()
	for range 3 {
		release, ok := limiter.TryAcquire()
		if !ok {
			t.Fatal("expected to acquire all limiter slots")
		}
		releases = append(releases, release)
	}

	if _, ok := limiter.TryAcquire(); ok {
		t.Fatal("expected acquisition to fail once the limit is reached")
	}

	releases[0]()
	if _, ok := limiter.TryAcquire(); !ok {
		t.Fatal("expected acquisition to succeed after a slot was released")
	}
}

func TestProbeConcurrencyLimitDisabled(t *testing.T) {
	limiter := NewProbeConcurrencyLimit(0)

	for range 1000 {
		release, ok := limiter.TryAcquire()
		if !ok {
			t.Fatal("a disabled limiter must never reject an acquisition")
		}
		defer release()
	}

	var nilLimiter *ProbeConcurrencyLimit
	if _, ok := nilLimiter.TryAcquire(); !ok {
		t.Fatal("a nil limiter must behave as disabled")
	}
}

func TestHandlerRejectsProbesOverConcurrencyLimit(t *testing.T) {
	// A slow target keeps the two allowed probe slots occupied.
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	t.Cleanup(target.Close)

	cfg := &config.Config{
		Modules: map[string]config.Module{
			"http_2xx": {
				Prober:  "http",
				Timeout: 2 * time.Second,
				HTTP: config.HTTPProbe{
					IPProtocolFallback: true,
				},
			},
		},
	}
	limiter := NewProbeConcurrencyLimit(2)

	serve := func() {
		req := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL, nil)
		rr := httptest.NewRecorder()
		Handler(rr, req, cfg, promslog.NewNopLogger(), &ResultHistory{}, 0.5, nil, nil, limiter, &promslog.Config{})
	}

	var occupied sync.WaitGroup
	for range 2 {
		occupied.Go(func() {
			serve()
		})
	}

	// Wait until both slots are held by the in-flight probes.
	deadline := time.Now().Add(2 * time.Second)
	full := false
	for time.Now().Before(deadline) {
		if _, ok := limiter.TryAcquire(); !ok {
			full = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !full {
		t.Fatal("limiter never reached capacity")
	}

	req := httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL, nil)
	rr := httptest.NewRecorder()
	Handler(rr, req, cfg, promslog.NewNopLogger(), &ResultHistory{}, 0.5, nil, nil, limiter, &promslog.Config{})

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d when the limit is reached, got %d", http.StatusServiceUnavailable, rr.Code)
	}

	occupied.Wait()

	// Once the in-flight probes finish, probes are accepted again.
	req = httptest.NewRequest(http.MethodGet, "/probe?target="+target.URL, nil)
	rr = httptest.NewRecorder()
	Handler(rr, req, cfg, promslog.NewNopLogger(), &ResultHistory{}, 0.5, nil, nil, limiter, &promslog.Config{})
	if rr.Code == http.StatusServiceUnavailable {
		t.Fatal("probe was rejected after the limiter capacity freed up")
	}
}
