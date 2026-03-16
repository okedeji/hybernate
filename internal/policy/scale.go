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
	"sync"
	"time"

	"github.com/okedeji/hybernate/internal/signal"
)

type ScaleDirection int

const (
	ScaleNone ScaleDirection = iota
	ScaleUp
	ScaleDown
)

func (d ScaleDirection) String() string {
	switch d {
	case ScaleUp:
		return "up"
	case ScaleDown:
		return "down"
	default:
		return "none"
	}
}

type ScaleDecision struct {
	Target    int32
	Current   int32
	Direction ScaleDirection
	Clamped   bool
	Reason    string
}

func (d ScaleDecision) GetTarget() int32  { return d.Target }
func (d ScaleDecision) ShouldScale() bool { return d.Direction != ScaleNone }
func (d ScaleDecision) String() string    { return d.Reason }

type ScaleConstraints struct {
	MinReplicas       int32
	MaxReplicas       int32
	DownStabilization time.Duration
	UpStabilization   time.Duration
	MaxStepDown       int32
	MaxStepUp         int32
}

type Scaler struct {
	Clock func() time.Time

	mu        sync.Mutex
	lastScale map[string]scaleEvent
}

type scaleEvent struct {
	at        time.Time
	direction ScaleDirection
}

func NewScaler() *Scaler {
	return &Scaler{
		Clock:     time.Now,
		lastScale: make(map[string]scaleEvent),
	}
}

// Evaluate takes a prediction-proposed target and the current replica count,
// checks signal consensus, applies stabilization and step limits, and returns
// a scaling decision.
func (s *Scaler) Evaluate(ctx context.Context, namespace, name string, proposed, current int32, constraints ScaleConstraints, signals []signal.Checker) (ScaleDecision, error) {
	if proposed == current {
		return ScaleDecision{
			Target:    current,
			Current:   current,
			Direction: ScaleNone,
			Reason:    "proposed matches current",
		}, nil
	}

	res, err := signal.CheckAll(ctx, namespace, name, signals)
	if err != nil {
		return ScaleDecision{}, err
	}
	if !res.Confirm {
		return ScaleDecision{
			Target:    current,
			Current:   current,
			Direction: ScaleNone,
			Reason:    res.Reason,
		}, nil
	}

	dir := ScaleUp
	if proposed < current {
		dir = ScaleDown
	}

	if s.inStabilization(namespace, name, dir, constraints) {
		return ScaleDecision{
			Target:    current,
			Current:   current,
			Direction: ScaleNone,
			Reason:    "in stabilization window",
		}, nil
	}

	target := clamp(proposed, constraints.MinReplicas, constraints.MaxReplicas)
	target = applyMaxStep(current, target, dir, constraints)
	target = clamp(target, constraints.MinReplicas, constraints.MaxReplicas)

	clamped := target != proposed

	if target == current {
		return ScaleDecision{
			Target:    current,
			Current:   current,
			Direction: ScaleNone,
			Clamped:   clamped,
			Reason:    "target equals current after constraints",
		}, nil
	}

	s.recordScale(namespace, name, dir)

	return ScaleDecision{
		Target:    target,
		Current:   current,
		Direction: dir,
		Clamped:   clamped,
		Reason:    res.Reason,
	}, nil
}

func (s *Scaler) inStabilization(namespace, name string, dir ScaleDirection, c ScaleConstraints) bool {
	key := workloadKey(namespace, name)

	s.mu.Lock()
	last, ok := s.lastScale[key]
	s.mu.Unlock()

	if !ok || last.direction != dir {
		return false
	}

	now := s.Clock()
	switch dir {
	case ScaleDown:
		return c.DownStabilization > 0 && now.Sub(last.at) < c.DownStabilization
	case ScaleUp:
		return c.UpStabilization > 0 && now.Sub(last.at) < c.UpStabilization
	}
	return false
}

func (s *Scaler) recordScale(namespace, name string, dir ScaleDirection) {
	key := workloadKey(namespace, name)
	s.mu.Lock()
	s.lastScale[key] = scaleEvent{at: s.Clock(), direction: dir}
	s.mu.Unlock()
}

func clamp(val, min, max int32) int32 {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

func applyMaxStep(current, target int32, dir ScaleDirection, c ScaleConstraints) int32 {
	diff := target - current
	if dir == ScaleDown && c.MaxStepDown > 0 && -diff > c.MaxStepDown {
		return current - c.MaxStepDown
	}
	if dir == ScaleUp && c.MaxStepUp > 0 && diff > c.MaxStepUp {
		return current + c.MaxStepUp
	}
	return target
}
