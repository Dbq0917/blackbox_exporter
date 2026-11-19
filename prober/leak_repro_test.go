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
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

// tlsHangServer accepts raw TCP connections and never replies, so a client
// TLS handshake hangs waiting for the ServerHello until its context ends.
func tlsHangServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln
}

// clientConns returns the number of live, client-side TCP connections whose
// remote end is listenerPort. When listenerPort is 0 every ESTABLISHED or
// CLOSE_WAIT socket is counted. Sockets owned by the in-process test server
// (whose local port is the listener) are excluded, so the measurement only
// reflects connections the prober still holds.
func clientConns(t *testing.T, listenerPort int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/net/tcp")
	if err != nil {
		t.Skipf("cannot read /proc/self/net/tcp: %s", err)
	}
	portSuffix := fmt.Sprintf(":%04X", listenerPort)
	count := 0
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		state, err := strconv.ParseInt(fields[3], 16, 32)
		if err != nil || (state != 1 && state != 8) {
			continue
		}
		if listenerPort > 0 {
			if !strings.HasSuffix(fields[2], portSuffix) {
				continue
			}
			// Skip the test server's own accepted sockets.
			if strings.HasSuffix(fields[1], portSuffix) {
				continue
			}
		}
		count++
	}
	return count
}

// waitForConns polls until clientConns drops to want or the deadline passes.
func waitForConns(t *testing.T, listenerPort, want int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := clientConns(t, listenerPort)
	for got > want && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		got = clientConns(t, listenerPort)
	}
	return got
}

// TestReproUnreachableTCPLeak runs many plain TCP probes against an
// unreachable (black-hole) target with a short probe timeout and checks that
// no client connection survives the batch.
func TestReproUnreachableTCPLeak(t *testing.T) {
	before := clientConns(t, 0)

	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			ProbeTCP(ctx, "192.0.2.1:80", config.Module{
				TCP: config.TCPProbe{IPProtocol: "ip4"},
			}, prometheus.NewRegistry(), promslog.NewNopLogger())
		})
	}
	wg.Wait()

	after := waitForConns(t, 0, before)
	t.Logf("TCP black-hole: client conns before=%d after=%d leaked=%d", before, after, after-before)
	if after > before {
		t.Fatalf("plain TCP probes leaked %d client connections", after-before)
	}
}

// TestReproUnreachableTLSLeak is the TLS variant of
// TestReproUnreachableTCPLeak.
func TestReproUnreachableTLSLeak(t *testing.T) {
	before := clientConns(t, 0)

	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			ProbeTCP(ctx, "192.0.2.1:443", config.Module{
				TCP: config.TCPProbe{
					IPProtocol: "ip4",
					TLS:        true,
					TLSConfig:  pconfig.TLSConfig{InsecureSkipVerify: true},
				},
			}, prometheus.NewRegistry(), promslog.NewNopLogger())
		})
	}
	wg.Wait()

	after := waitForConns(t, 0, before)
	t.Logf("TLS black-hole: client conns before=%d after=%d leaked=%d", before, after, after-before)
	if after > before {
		t.Fatalf("TLS probes leaked %d client connections", after-before)
	}
}

// startTLSHangServer accepts connections, walks the plaintext part of an
// SMTP-like STARTTLS dialog and then never answers the TLS handshake. The
// handler returns as soon as the peer closes its side, so any surviving
// client socket belongs to a probe that failed to release its connection.
func startTLSHangServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				fmt.Fprintf(conn, "220 pseudo ESMTP\n")
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if string(buf[:n]) == "STARTTLS\n" {
						fmt.Fprintf(conn, "220 ready\n")
						// Never respond to the TLS handshake. The next read
						// only returns once the client closes its side.
						if _, err := conn.Read(buf); err != nil {
							return
						}
					}
				}
			}(conn)
		}
	}()
	return ln
}

// TestReproStartTLSHandshakeLeak checks that a probe whose context expires
// while the STARTTLS handshake is in flight still closes its socket.
func TestReproStartTLSHandshakeLeak(t *testing.T) {
	ln := startTLSHangServer(t)
	defer ln.Close()
	listenerPort := ln.Addr().(*net.TCPAddr).Port

	module := config.Module{
		TCP: config.TCPProbe{
			IPProtocol: "ip4",
			QueryResponse: []config.QueryResponse{
				{Expect: config.MustNewRegexp("^220")},
				{Send: "STARTTLS"},
				{Expect: config.MustNewRegexp("^220")},
				{StartTLS: true},
			},
			TLSConfig: pconfig.TLSConfig{InsecureSkipVerify: true},
		},
	}

	before := clientConns(t, listenerPort)

	const n = 100
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			ProbeTCP(ctx, ln.Addr().String(), module, prometheus.NewRegistry(), promslog.NewNopLogger())
		})
	}
	wg.Wait()

	after := waitForConns(t, listenerPort, before)
	t.Logf("STARTTLS hang: client conns before=%d after=%d leaked=%d", before, after, after-before)
	if after > before {
		t.Fatalf("STARTTLS probes leaked %d client connections", after-before)
	}
}

// TestReproStartTLSHandshakeCancellation drives the STARTTLS path with a
// context that has no deadline and is cancelled explicitly. A correct
// implementation must interrupt the handshake and close the socket on
// cancellation rather than only expiring the wait. The goroutine guard turns
// an unbounded hang (the original behaviour) into a fast, hermetic failure.
func TestReproStartTLSHandshakeCancellation(t *testing.T) {
	ln := startTLSHangServer(t)
	defer ln.Close()
	listenerPort := ln.Addr().(*net.TCPAddr).Port

	module := config.Module{
		TCP: config.TCPProbe{
			IPProtocol: "ip4",
			QueryResponse: []config.QueryResponse{
				{Expect: config.MustNewRegexp("^220")},
				{Send: "STARTTLS"},
				{Expect: config.MustNewRegexp("^220")},
				{StartTLS: true},
			},
			TLSConfig: pconfig.TLSConfig{InsecureSkipVerify: true},
		},
	}

	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()
		ProbeTCP(ctx, ln.Addr().String(), module, prometheus.NewRegistry(), promslog.NewNopLogger())
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not return after its context was cancelled")
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("probe returned after %s, before cancellation; it did not block on the handshake", elapsed)
	}
	t.Logf("probe returned %s after start, cancellation fired at 200ms", time.Since(started))

	after := waitForConns(t, listenerPort, 0)
	if after != 0 {
		t.Fatalf("cancelled STARTTLS probe left %d client connection(s) open", after)
	}
}

// TestReproTLSDialCancellation dials TLS directly with a context that is
// cancelled without a deadline. The dial path must interrupt the handshake
// and close the socket rather than waiting indefinitely.
func TestReproTLSDialCancellation(t *testing.T) {
	ln := tlsHangServer(t)
	defer ln.Close()
	listenerPort := ln.Addr().(*net.TCPAddr).Port

	module := config.Module{
		TCP: config.TCPProbe{
			IPProtocol: "ip4",
			TLS:        true,
			TLSConfig:  pconfig.TLSConfig{InsecureSkipVerify: true},
		},
	}

	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()
		ProbeTCP(ctx, ln.Addr().String(), module, prometheus.NewRegistry(), promslog.NewNopLogger())
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("TLS probe did not return after its context was cancelled")
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("probe returned after %s, before cancellation", elapsed)
	}

	if after := waitForConns(t, listenerPort, 0); after != 0 {
		t.Fatalf("cancelled TLS probe left %d client connection(s) open", after)
	}
}
