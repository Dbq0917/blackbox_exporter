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
	"time"
)

// probeContext is a context whose deadline can be moved while a probe is
// running. Its Done channel is closed once the deadline passes, the parent is
// canceled, or the probe finishes.
type probeContext struct {
	parent context.Context
	done   chan struct{}

	mu       sync.Mutex
	deadline time.Time
	err      error
	finished bool
}

func newProbeContext(parent context.Context, deadline time.Time) *probeContext {
	return &probeContext{
		parent:   parent,
		done:     make(chan struct{}),
		deadline: effectiveDeadline(parent, deadline),
	}
}

func effectiveDeadline(parent context.Context, deadline time.Time) time.Time {
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		return parentDeadline
	}
	return deadline
}

// Deadline returns the current deadline, which can change while the probe is
// running.
func (c *probeContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, !c.deadline.IsZero()
}

func (c *probeContext) Done() <-chan struct{} {
	return c.done
}

func (c *probeContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *probeContext) Value(key any) any {
	return c.parent.Value(key)
}

func (c *probeContext) setDeadline(deadline time.Time) {
	c.mu.Lock()
	c.deadline = effectiveDeadline(c.parent, deadline)
	c.mu.Unlock()
}

func (c *probeContext) finish(err error) {
	c.mu.Lock()
	if !c.finished {
		c.finished = true
		c.err = err
		close(c.done)
	}
	c.mu.Unlock()
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
