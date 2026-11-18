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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// capNetRaw is the bit index of CAP_NET_RAW in the kernel's capability masks.
const capNetRaw = 13

// isSocketPermissionError reports whether err indicates that the process
// lacks the privileges to open the requested socket, as opposed to the
// target being unreachable or the address being invalid.
func isSocketPermissionError(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// hasCapNetRaw reports whether CAP_NET_RAW is in the process' effective
// capability set.
func hasCapNetRaw() bool {
	caps, err := effectiveCapabilities()
	if err != nil {
		return false
	}
	return caps&(1<<capNetRaw) != 0
}

// effectiveCapabilities parses CapEff from /proc/self/status.
func effectiveCapabilities() (uint64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		}
	}
	return 0, errors.New("CapEff not found in /proc/self/status")
}

// pingGroupRange returns the low and high group IDs allowed to create
// unprivileged ICMP echo sockets, as configured by the
// net.ipv4.ping_group_range sysctl.
func pingGroupRange() (low, high int64, err error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ping_group_range")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ping_group_range format %q", strings.TrimSpace(string(data)))
	}
	if low, err = strconv.ParseInt(fields[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("parsing ping_group_range: %w", err)
	}
	if high, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("parsing ping_group_range: %w", err)
	}
	return low, high, nil
}

// icmpSocketPermissionReason returns a human-readable diagnosis for a
// failed ICMP socket creation, naming the exact missing capability and the
// state of the unprivileged ping socket fallback. It returns an empty
// string if err is not a permission error.
func icmpSocketPermissionReason(err error) string {
	if !isSocketPermissionError(err) {
		return ""
	}

	var b strings.Builder
	if hasCapNetRaw() {
		b.WriteString("permission denied even though CAP_NET_RAW is in the effective capability set")
	} else {
		b.WriteString("permission denied: process lacks CAP_NET_RAW, required to open raw ICMP sockets")
	}

	egid := int64(os.Getegid())
	if low, high, perr := pingGroupRange(); perr != nil {
		fmt.Fprintf(&b, "; could not read net.ipv4.ping_group_range: %v", perr)
	} else if low > high || egid < low || egid > high {
		fmt.Fprintf(&b, "; unprivileged ping sockets are not available to gid %d (net.ipv4.ping_group_range is \"%d %d\")", egid, low, high)
	} else {
		fmt.Fprintf(&b, "; unprivileged ping sockets should be available to gid %d (net.ipv4.ping_group_range is \"%d %d\")", egid, low, high)
	}

	b.WriteString(`; grant CAP_NET_RAW (e.g. "setcap cap_net_raw+ep blackbox_exporter" or "docker run --cap-add NET_RAW") or allow unprivileged ping sockets via net.ipv4.ping_group_range`)
	return b.String()
}
