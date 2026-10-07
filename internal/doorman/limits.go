/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package doorman

import (
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/types"
)

// limits bound what callers can make one doorman pod do. A held connection
// costs two goroutines and its first bytes, about 20KiB, so maxHeld keeps
// held connections well inside the pod's memory.
//
// An ingress controller sends every user's traffic from one address, so a
// source's held connections are capped per workload: a burst of requests to
// one environment can't crowd out the others behind the same ingress. A
// source's total is capped too, at half of maxHeld, so a single client
// can't fill the doorman however many workloads it reaches.
//
// Wakes are limited per source, so a client scanning the doorman's ports
// wakes only a few workloads at once, and overall. A caller over a limit
// isn't turned away: it's held, or shown the waking-up page, and the wake is
// retried as the allowance refills.
//
// What held callers send while they wait is buffered, so that one who
// leaves is noticed (see clientStream): up to maxBufferedPerConn each,
// enough for most request bodies, and maxBuffered across them all, which
// bounds the memory a flood of large uploads can take.
type limits struct {
	maxHeld                  int
	maxHeldPerSource         int
	maxHeldPerSourceWorkload int

	maxBufferedPerConn int
	maxBuffered        int

	sourceWakeEvery time.Duration
	sourceWakeBurst int
	wakeEvery       time.Duration
	wakeBurst       int
}

var defaultLimits = limits{
	maxHeld:                  2048,
	maxHeldPerSource:         1024,
	maxHeldPerSourceWorkload: 256,
	maxBufferedPerConn:       1 << 20,
	maxBuffered:              32 << 20,
	sourceWakeEvery:          2 * time.Second,
	sourceWakeBurst:          30,
	wakeEvery:                100 * time.Millisecond,
	wakeBurst:                200,
}

// sourceIdle is how long a source with nothing held is remembered. Its wake
// allowance has long refilled by then.
const sourceIdle = 10 * time.Minute

type source struct {
	held       int
	byWorkload map[types.NamespacedName]int
	wakes      *rate.Limiter
	seen       time.Time
}

// admission counts held connections and wakes, overall and per source.
type admission struct {
	limits limits

	mu       sync.Mutex
	held     int
	buffered int
	sources  map[netip.Addr]*source
	wakes    *rate.Limiter
}

func newAdmission(l limits) *admission {
	return &admission{
		limits:  l,
		sources: map[netip.Addr]*source{},
		wakes:   rate.NewLimiter(rate.Every(l.wakeEvery), l.wakeBurst),
	}
}

func (a *admission) source(addr netip.Addr, now time.Time) *source {
	s, ok := a.sources[addr]
	if !ok {
		s = &source{
			byWorkload: map[types.NamespacedName]int{},
			wakes:      rate.NewLimiter(rate.Every(a.limits.sourceWakeEvery), a.limits.sourceWakeBurst),
		}
		a.sources[addr] = s
	}
	s.seen = now
	return s
}

// hold admits one more connection from addr held for workload, unless a cap
// is reached.
func (a *admission) hold(addr netip.Addr, workload types.NamespacedName, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.source(addr, now)
	if a.held >= a.limits.maxHeld || s.held >= a.limits.maxHeldPerSource ||
		s.byWorkload[workload] >= a.limits.maxHeldPerSourceWorkload {
		return false
	}
	a.held++
	s.held++
	s.byWorkload[workload]++
	return true
}

// release ends a held connection hold admitted.
func (a *admission) release(addr netip.Addr, workload types.NamespacedName, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held--
	s := a.source(addr, now)
	s.held--
	if s.byWorkload[workload]--; s.byWorkload[workload] <= 0 {
		delete(s.byWorkload, workload)
	}
}

// allowWake reports whether addr may start another wake now. Neither
// allowance is spent unless both have room, so a wake refused overall
// doesn't use up the source's.
func (a *admission) allowWake(addr netip.Addr, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.source(addr, now)
	if s.wakes.TokensAt(now) < 1 || a.wakes.TokensAt(now) < 1 {
		return false
	}
	return s.wakes.AllowN(now, 1) && a.wakes.AllowN(now, 1)
}

// reserveBuffer takes n bytes of the held connections' buffer budget, if
// they're free.
func (a *admission) reserveBuffer(n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buffered+n > a.limits.maxBuffered {
		return false
	}
	a.buffered += n
	return true
}

func (a *admission) releaseBuffer(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buffered -= n
}

// prune forgets sources with nothing held that haven't been seen lately.
func (a *admission) prune(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for addr, s := range a.sources {
		if s.held == 0 && now.Sub(s.seen) > sourceIdle {
			delete(a.sources, addr)
		}
	}
}
