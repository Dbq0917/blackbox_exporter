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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

func writeProbeConfig(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}
}

func newProbeTestRequest(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/probe?target="+url.QueryEscape(target), nil)
}

func runProbeHandler(t *testing.T, sc *config.SafeConfig, target string) (*httptest.ResponseRecorder, <-chan struct{}) {
	t.Helper()
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandlerWithConfigSource(w, r, sc, promslog.NewNopLogger(), &ResultHistory{}, 0.5, nil, nil, &promslog.Config{})
	})

	go func() {
		defer close(done)
		handler.ServeHTTP(rr, newProbeTestRequest(target))
	}()
	return rr, done
}

func TestProbeUsesReducedTimeoutAfterReload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	closeRelease := func() {
		releaseOnce.Do(func() { close(release) })
	}

	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer closeRelease()

	configFile := filepath.Join(t.TempDir(), "blackbox.yml")
	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 10s
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)

	sc := config.NewSafeConfig(prometheus.NewRegistry())
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	rr, done := runProbeHandler(t, sc, ts.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe did not reach the target")
	}

	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 100ms
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe kept using the old timeout after reload")
	}

	body := rr.Body.String()
	if !strings.Contains(body, "probe_success 0") {
		t.Errorf("Probe did not fail after the timeout was reduced: %s", body)
	}
	if !strings.Contains(body, "probe_timeout_seconds 0.1") {
		t.Errorf("Probe did not report the reduced timeout: %s", body)
	}
}

func TestProbeUsesExtendedTimeoutAfterReload(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			return
		case <-time.After(650 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	configFile := filepath.Join(t.TempDir(), "blackbox.yml")
	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 500ms
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)

	sc := config.NewSafeConfig(prometheus.NewRegistry())
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	rr, done := runProbeHandler(t, sc, ts.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe did not reach the target")
	}

	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 2s
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe did not finish")
	}

	body := rr.Body.String()
	if !strings.Contains(body, "probe_success 1") {
		t.Errorf("Probe was cut off by the old timeout: %s", body)
	}
	if !strings.Contains(body, "probe_timeout_seconds 2") {
		t.Errorf("Probe did not report the extended timeout: %s", body)
	}
}

func TestProbeUnaffectedByOtherModuleReload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	closeRelease := func() {
		releaseOnce.Do(func() { close(release) })
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			return
		case <-release:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()
	defer closeRelease()

	configFile := filepath.Join(t.TempDir(), "blackbox.yml")
	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 10s
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
  other:
    prober: http
    timeout: 10s
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)

	sc := config.NewSafeConfig(prometheus.NewRegistry())
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	rr, done := runProbeHandler(t, sc, ts.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe did not reach the target")
	}

	writeProbeConfig(t, configFile, `
modules:
  http_2xx:
    prober: http
    timeout: 10s
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
  other:
    prober: http
    timeout: 100ms
    http:
      preferred_ip_protocol: ip4
      ip_protocol_fallback: false
`)
	if err := sc.ReloadConfig(configFile, nil); err != nil {
		t.Fatalf("Failed to reload config: %v", err)
	}

	closeRelease()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Probe was interrupted by an unrelated module update")
	}

	body := rr.Body.String()
	if !strings.Contains(body, "probe_success 1") {
		t.Errorf("Probe was interrupted by an unrelated module update: %s", body)
	}
	if !strings.Contains(body, "probe_timeout_seconds 10") {
		t.Errorf("Probe did not keep its timeout: %s", body)
	}
}
