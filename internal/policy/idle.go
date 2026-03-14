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

package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/okedeji/hybernate/internal/signal"
)

type IdleStatus int

const (
	IdleStatusActive IdleStatus = iota
	IdleStatusIdle
)

func (s IdleStatus) String() string {
	switch s {
	case IdleStatusActive:
		return "Active"
	case IdleStatusIdle:
		return "Idle"
	default:
		return "Unknown"
	}
}

type IdleEvaluation struct {
	Status  IdleStatus
	IdleFor time.Duration
	Reasons []string
}

type IdleDetector struct {
	Clock func() time.Time

	mu        sync.Mutex
	idleSince map[string]time.Time
}

func NewIdleDetector() *IdleDetector {
	return &IdleDetector{
		Clock:     time.Now,
		idleSince: make(map[string]time.Time),
	}
}

// Evaluate checks all signals and determines whether the workload has been
// idle long enough to act on. Every signal must confirm idleness; if any
// signal denies, the idle timer resets.
func (d *IdleDetector) Evaluate(ctx context.Context, namespace, name string, signals []signal.Checker, timeout time.Duration) (IdleEvaluation, error) {
	if len(signals) == 0 {
		return IdleEvaluation{Status: IdleStatusActive, Reasons: []string{"no signals configured"}}, nil
	}

	key := workloadKey(namespace, name)

	res, err := signal.CheckAll(ctx, namespace, name, signals)
	if err != nil {
		return IdleEvaluation{}, err
	}
	if !res.Confirm {
		d.mu.Lock()
		delete(d.idleSince, key)
		d.mu.Unlock()
		return IdleEvaluation{Status: IdleStatusActive, Reasons: []string{res.Reason}}, nil
	}

	now := d.Clock()

	d.mu.Lock()
	defer d.mu.Unlock()

	since, tracked := d.idleSince[key]
	if !tracked {
		d.idleSince[key] = now
		return IdleEvaluation{
			Status:  IdleStatusActive,
			Reasons: []string{"all signals confirm idle, starting idle timer"},
		}, nil
	}

	idleFor := now.Sub(since)
	if idleFor >= timeout {
		return IdleEvaluation{
			Status:  IdleStatusIdle,
			IdleFor: idleFor,
			Reasons: []string{fmt.Sprintf("idle for %s, exceeds timeout %s", idleFor, timeout)},
		}, nil
	}

	return IdleEvaluation{
		Status:  IdleStatusActive,
		IdleFor: idleFor,
		Reasons: []string{fmt.Sprintf("idle for %s, waiting for timeout %s", idleFor, timeout)},
	}, nil
}

// Reset clears the idle timer for a workload. Call when a workload
// transitions out of idle (resumed, scaled up, etc).
func (d *IdleDetector) Reset(namespace, name string) {
	key := workloadKey(namespace, name)
	d.mu.Lock()
	delete(d.idleSince, key)
	d.mu.Unlock()
}

func (e IdleEvaluation) String() string {
	if e.Status == IdleStatusActive {
		return fmt.Sprintf("Active (%s)", strings.Join(e.Reasons, "; "))
	}
	return fmt.Sprintf("Idle for %s (%s)", e.IdleFor, strings.Join(e.Reasons, "; "))
}

func workloadKey(namespace, name string) string {
	return namespace + "/" + name
}
