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

//go:build linux

package prober

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
)

func testICMPModule(dontFragment bool) config.Module {
	return config.Module{
		Timeout: 5 * time.Second,
		ICMP: config.ICMPProbe{
			IPProtocol:         "ip4",
			IPProtocolFallback: false,
			DontFragment:       dontFragment,
		},
	}
}

func testICMPLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func probeLocalhostICMP(t *testing.T, module config.Module, buf *bytes.Buffer) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), module.Timeout)
	defer cancel()
	return ProbeICMP(ctx, "127.0.0.1", module, prometheus.NewRegistry(), testICMPLogger(buf))
}

// TestICMPSocketPermissionReason checks that permission errors are diagnosed
// with the exact missing capability, while other errors are left alone.
func TestICMPSocketPermissionReason(t *testing.T) {
	for _, err := range []error{syscall.EPERM, syscall.EACCES, fmt.Errorf("listen ip4:icmp: %w", syscall.EPERM)} {
		reason := icmpSocketPermissionReason(err)
		if reason == "" {
			t.Fatalf("expected a reason for %v, got none", err)
		}
		if !strings.Contains(reason, "permission denied") {
			t.Errorf("reason for %v does not mention permission: %q", err, reason)
		}
		if !hasCapNetRaw() && !strings.Contains(reason, "CAP_NET_RAW") {
			t.Errorf("reason for %v does not name the missing capability: %q", err, reason)
		}
		if !strings.Contains(reason, "ping_group_range") {
			t.Errorf("reason for %v does not cover the unprivileged fallback: %q", err, reason)
		}
	}

	if reason := icmpSocketPermissionReason(syscall.ECONNREFUSED); reason != "" {
		t.Errorf("expected no reason for non-permission error, got %q", reason)
	}
}

// TestICMPProbeRawSocketPermissionDenied runs the probe forced onto the raw
// socket path (dont_fragment) in an environment without CAP_NET_RAW and
// asserts that the failure is reported as a permission problem naming the
// missing capability, not as an unreachable target.
//
// This is the "no raw socket permission" half of the acceptance check; the
// container default (no CAP_NET_RAW) satisfies it.
func TestICMPProbeRawSocketPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 || hasCapNetRaw() {
		t.Skip("test requires an environment without raw socket privileges (no root, no CAP_NET_RAW)")
	}

	var buf bytes.Buffer
	if success := probeLocalhostICMP(t, testICMPModule(true), &buf); success {
		t.Fatal("probe unexpectedly succeeded without raw socket privileges")
	}

	logs := buf.String()
	if !strings.Contains(logs, "permission denied") {
		t.Errorf("logs do not identify the failure as a permission problem:\n%s", logs)
	}
	if !strings.Contains(logs, "CAP_NET_RAW") {
		t.Errorf("logs do not name the missing capability:\n%s", logs)
	}
	if strings.Contains(logs, "Timeout") || strings.Contains(logs, "unreachable") {
		t.Errorf("logs mislead towards an unreachable target:\n%s", logs)
	}
	t.Logf("recorded permission error sample:\n%s", logs)
}

// TestICMPProbeUnprivilegedFallback checks that the probe degrades to
// unprivileged ping sockets when raw sockets are not permitted but
// net.ipv4.ping_group_range allows the process group.
func TestICMPProbeUnprivilegedFallback(t *testing.T) {
	low, high, err := pingGroupRange()
	if err != nil {
		t.Skipf("cannot read ping_group_range: %v", err)
	}
	if egid := int64(os.Getegid()); low > high || egid < low || egid > high {
		t.Skipf("unprivileged ping sockets not available to gid %d (ping_group_range %d %d)", egid, low, high)
	}

	var buf bytes.Buffer
	if success := probeLocalhostICMP(t, testICMPModule(false), &buf); !success {
		t.Fatalf("unprivileged fallback probe failed:\n%s", buf.String())
	}
}

// TestICMPProbeRawSocketPermissionGranted is the "sufficient privileges"
// half of the acceptance check: with CAP_NET_RAW (or root) the raw socket
// probe must succeed. Run it with e.g.:
//
//	go test -c ./prober && sudo setcap cap_net_raw+ep prober.test &&
//	./prober.test -test.run TestICMPProbeRawSocketPermissionGranted
//
// or simply: sudo go test -run TestICMPProbeRawSocketPermissionGranted ./prober
func TestICMPProbeRawSocketPermissionGranted(t *testing.T) {
	if os.Geteuid() != 0 && !hasCapNetRaw() {
		t.Skip("test requires CAP_NET_RAW or root; see test comment for how to run it")
	}

	var buf bytes.Buffer
	if success := probeLocalhostICMP(t, testICMPModule(true), &buf); !success {
		t.Fatalf("raw socket probe failed despite privileges:\n%s", buf.String())
	}
}
