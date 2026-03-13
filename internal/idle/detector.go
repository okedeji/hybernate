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

package idle

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Status int

const (
	Active Status = iota
	Idle
)

func (s Status) String() string {
	switch s {
	case Active:
		return "Active"
	case Idle:
		return "Idle"
	default:
		return "Unknown"
	}
}

type Evaluation struct {
	Status  Status
	IdleFor time.Duration
	Reasons []string
}

type Detector struct {
	Clock func() time.Time

	mu        sync.Mutex
	idleSince map[string]time.Time
}

func NewDetector() *Detector {
	return &Detector{
		Clock:     time.Now,
		idleSince: make(map[string]time.Time),
	}
}

func workloadKey(namespace, name string) string {
	return namespace + "/" + name
}

// Evaluate checks all signals and determines whether the workload has been
// idle long enough to act on. Every signal must report idle; if any signal
// reports active, the idle timer resets.
func (d *Detector) Evaluate(ctx context.Context, namespace, name string, signals []Signal, timeout time.Duration) (Evaluation, error) {
	if len(signals) == 0 {
		return Evaluation{Status: Active, Reasons: []string{"no signals configured"}}, nil
	}

	key := workloadKey(namespace, name)

	for _, sig := range signals {
		res, err := sig.Check(ctx, namespace, name)
		if err != nil {
			return Evaluation{}, fmt.Errorf("checking signal for %s/%s: %w", namespace, name, err)
		}
		if res.Active {
			d.mu.Lock()
			delete(d.idleSince, key)
			d.mu.Unlock()
			return Evaluation{Status: Active, Reasons: []string{res.Reason}}, nil
		}
	}

	now := d.Clock()

	d.mu.Lock()
	defer d.mu.Unlock()

	since, tracked := d.idleSince[key]
	if !tracked {
		d.idleSince[key] = now
		return Evaluation{
			Status:  Active,
			Reasons: []string{"all signals report idle, starting idle timer"},
		}, nil
	}

	idleFor := now.Sub(since)
	if idleFor >= timeout {
		return Evaluation{
			Status:  Idle,
			IdleFor: idleFor,
			Reasons: []string{fmt.Sprintf("idle for %s, exceeds timeout %s", idleFor, timeout)},
		}, nil
	}

	return Evaluation{
		Status:  Active,
		IdleFor: idleFor,
		Reasons: []string{fmt.Sprintf("idle for %s, waiting for timeout %s", idleFor, timeout)},
	}, nil
}

// Reset clears the idle timer for a workload. Call when a workload
// transitions out of idle (resumed, scaled up, etc).
func (d *Detector) Reset(namespace, name string) {
	key := workloadKey(namespace, name)
	d.mu.Lock()
	delete(d.idleSince, key)
	d.mu.Unlock()
}

func (e Evaluation) String() string {
	if e.Status == Active {
		return fmt.Sprintf("Active (%s)", strings.Join(e.Reasons, "; "))
	}
	return fmt.Sprintf("Idle for %s (%s)", e.IdleFor, strings.Join(e.Reasons, "; "))
}
