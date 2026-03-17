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
	IdleStatusSignalsConfirm
	IdleStatusInGracePeriod
	IdleStatusIdle
)

func (s IdleStatus) String() string {
	switch s {
	case IdleStatusActive:
		return "Active"
	case IdleStatusSignalsConfirm:
		return "SignalsConfirm"
	case IdleStatusInGracePeriod:
		return "InGracePeriod"
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

	mu         sync.Mutex
	graceStart map[string]time.Time
}

func NewIdleDetector() *IdleDetector {
	return &IdleDetector{
		Clock:      time.Now,
		graceStart: make(map[string]time.Time),
	}
}

// Evaluate checks signals and manages the grace period.
//
// Flow:
//  1. If signals deny → Active (resets grace timer)
//  2. If signals confirm and no grace timer → SignalsConfirm (caller should
//     check prediction, then call StartGracePeriod if confirmed)
//  3. If signals confirm and grace timer running → InGracePeriod
//  4. If signals confirm and grace timer elapsed → Idle
//
// When gracePeriod is zero, transitions directly from SignalsConfirm to Idle
// on the next call after the caller starts the grace period.
func (d *IdleDetector) Evaluate(ctx context.Context, namespace, name string, signals []signal.Checker, gracePeriod time.Duration) (IdleEvaluation, error) {
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
		delete(d.graceStart, key)
		d.mu.Unlock()
		return IdleEvaluation{Status: IdleStatusActive, Reasons: []string{res.Reason}}, nil
	}

	now := d.Clock()

	d.mu.Lock()
	defer d.mu.Unlock()

	started, hasGrace := d.graceStart[key]
	if !hasGrace {
		return IdleEvaluation{
			Status:  IdleStatusSignalsConfirm,
			Reasons: []string{"signals confirm idle, awaiting prediction confirmation"},
		}, nil
	}

	elapsed := now.Sub(started)
	if gracePeriod > 0 && elapsed < gracePeriod {
		return IdleEvaluation{
			Status:  IdleStatusInGracePeriod,
			IdleFor: elapsed,
			Reasons: []string{fmt.Sprintf("in grace period, %s remaining", gracePeriod-elapsed)},
		}, nil
	}

	return IdleEvaluation{
		Status:  IdleStatusIdle,
		IdleFor: elapsed,
		Reasons: []string{"signals confirm idle, grace period elapsed"},
	}, nil
}

// StartGracePeriod begins the grace period timer. Call after prediction
// confirms the signals' idle detection.
func (d *IdleDetector) StartGracePeriod(namespace, name string) {
	key := workloadKey(namespace, name)
	d.mu.Lock()
	if _, ok := d.graceStart[key]; !ok {
		d.graceStart[key] = d.Clock()
	}
	d.mu.Unlock()
}

// Reset clears the grace period timer for a workload.
func (d *IdleDetector) Reset(namespace, name string) {
	key := workloadKey(namespace, name)
	d.mu.Lock()
	delete(d.graceStart, key)
	d.mu.Unlock()
}

func (e IdleEvaluation) IsIdle() bool                { return e.Status == IdleStatusIdle }
func (e IdleEvaluation) IdleDuration() time.Duration { return e.IdleFor }
func (e IdleEvaluation) SignalsConfirm() bool        { return e.Status == IdleStatusSignalsConfirm }
func (e IdleEvaluation) InGracePeriod() bool         { return e.Status == IdleStatusInGracePeriod }

func (e IdleEvaluation) String() string {
	return fmt.Sprintf("%s (%s)", e.Status, strings.Join(e.Reasons, "; "))
}

func workloadKey(namespace, name string) string {
	return namespace + "/" + name
}
