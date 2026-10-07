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
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

const (
	// wakeStampInterval is how long a stamp this replica made is trusted to
	// be on its way into the cache, so a burst of connections arriving
	// before it shows up doesn't patch the workload once each.
	wakeStampInterval = 10 * time.Second

	// unservedEventInterval keeps a burst of timed-out connections, all
	// waiting on the same slow wake, to one warning event.
	unservedEventInterval = time.Minute

	apiTimeout = 10 * time.Second
)

// errWakeLimited means the caller has started too many wakes lately.
var errWakeLimited = errors.New("too many wakes from this source")

// wakeCall is one stamp in flight, shared by every connection that needs
// the same workload woken at the same time.
type wakeCall struct {
	done   chan struct{}
	err    error
	source netip.Addr
}

// stampRecord is the last stamp this replica made for a workload, and the
// pause it woke.
type stampRecord struct {
	pausedAt time.Time
	at       time.Time
}

// requestWake makes sure the workload is woken: it stamps the last-request
// annotation unless a wake is already on its way. Connections for the same
// workload share one stamp. A failed stamp is returned, for the caller to
// retry while it holds the connection.
func (s *Server) requestWake(ctx context.Context, rt route, from netip.Addr) error {
	for {
		s.mu.Lock()
		call, inFlight := s.wakeCalls[rt.workload]
		if !inFlight {
			call = &wakeCall{done: make(chan struct{}), source: from}
			s.wakeCalls[rt.workload] = call
		}
		s.mu.Unlock()

		if inFlight {
			select {
			case <-call.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			// Another source's allowance says nothing about this one's.
			if errors.Is(call.err, errWakeLimited) && call.source != from {
				continue
			}
			return call.err
		}

		call.err = s.stamp(ctx, rt, from)
		s.mu.Lock()
		delete(s.wakeCalls, rt.workload)
		s.mu.Unlock()
		close(call.done)
		if call.err != nil && !errors.Is(call.err, errWakeLimited) {
			log.FromContext(ctx).Error(call.err, "waking workload",
				"workload", rt.workload.Name, "namespace", rt.workload.Namespace, "service", rt.service)
		}
		return call.err
	}
}

// stamp writes a new last-request value, and where the request came from, so
// the operator can learn what depends on the workload. The value always
// differs from the one recorded when the pause began, which is what the
// operator takes as a wake.
func (s *Server) stamp(ctx context.Context, rt route, from netip.Addr) error {
	// The stamp is for every connection waiting on it, not only the one
	// that made it, so it isn't cut short when that one goes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
	defer cancel()

	var w v1alpha1.ManagedWorkload
	if err := s.client.Get(ctx, rt.workload, &w); err != nil {
		return fmt.Errorf("getting managed workload: %w", err)
	}
	if !needsWake(&w) {
		return nil
	}
	now := s.now()
	pausedAt := pausedAt(&w)
	s.mu.Lock()
	last, stamped := s.stamped[rt.workload]
	s.mu.Unlock()
	if stamped && last.pausedAt.Equal(pausedAt) && now.Sub(last.at) < wakeStampInterval {
		return nil
	}
	if !s.admission.allowWake(from, now) {
		return errWakeLimited
	}

	original := w.DeepCopy()
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	value := now.UTC().Format(time.RFC3339)
	if value == w.Annotations[v1alpha1.AnnotationLastRequest] {
		value = now.UTC().Format(time.RFC3339Nano)
	}
	w.Annotations[v1alpha1.AnnotationLastRequest] = value
	w.Annotations[v1alpha1.AnnotationLastRequestFrom] = from.Unmap().String()
	// The stamp is locked to the version this replica read, so when two
	// stamp at once, only the first to land reports the wake. A conflict
	// means the workload changed a moment ago, most likely the other
	// replica's stamp; the stamp is repeated unlocked so the wake is never
	// lost, and the event is left to whoever won.
	err := s.client.Patch(ctx, &w, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	won := err == nil
	if apierrors.IsConflict(err) {
		err = s.client.Patch(ctx, &w, client.MergeFrom(original))
	}
	if err != nil {
		return fmt.Errorf("stamping last request: %w", err)
	}

	s.mu.Lock()
	s.stamped[rt.workload] = stampRecord{pausedAt: pausedAt, at: now}
	s.mu.Unlock()
	if s.recorder != nil && won {
		s.recorder.Eventf(&w, nil, "Normal", "WokenByRequest", "Wake",
			"request on Service %s, waking", rt.service)
	}
	return nil
}

// needsWake reports whether a paused workload has no wake pending: its
// last-request annotation is as it was when the pause began. A workload
// that isn't Paused is awake, or already waking.
func needsWake(w *v1alpha1.ManagedWorkload) bool {
	if w.Status.Phase != v1alpha1.PhasePaused {
		return false
	}
	current, set := w.Annotations[v1alpha1.AnnotationLastRequest]
	pause := w.Status.Pause
	if pause != nil && pause.WakeAnnotations != nil {
		recorded, wasSet := pause.WakeAnnotations.Workload[v1alpha1.AnnotationLastRequest]
		return set == wasSet && current == recorded
	}
	if !set {
		return true
	}
	stamped, err := time.Parse(time.RFC3339, current)
	if err != nil || pause == nil || pause.PausedAt == nil {
		return true
	}
	return stamped.Before(pause.PausedAt.Time)
}

func pausedAt(w *v1alpha1.ManagedWorkload) time.Time {
	if p := w.Status.Pause; p != nil && p.PausedAt != nil {
		return p.PausedAt.Time
	}
	return time.Time{}
}

// warnUnserved emits a warning event on the workload for a held connection
// that was closed without reaching it, at most once per unservedEventInterval.
// The metrics count every one; the event is what shows in kubectl describe.
func (s *Server) warnUnserved(ctx context.Context, rt route, result string, waited time.Duration) {
	if s.recorder == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	if last, ok := s.lastWarn[rt.workload]; ok && now.Sub(last) < unservedEventInterval {
		s.mu.Unlock()
		return
	}
	s.lastWarn[rt.workload] = now
	s.mu.Unlock()

	// The connection's context may be what ran out, so the lookup gets its own.
	getCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
	defer cancel()
	var w v1alpha1.ManagedWorkload
	if err := s.client.Get(getCtx, rt.workload, &w); err != nil {
		log.FromContext(ctx).Error(err, "getting managed workload for event",
			"workload", rt.workload.Name, "namespace", rt.workload.Namespace)
		return
	}
	reason := "the workload wasn't Ready within maxWait; the wake continues"
	if result != resultTimeout {
		reason = "the workload couldn't be reached"
	}
	s.recorder.Eventf(&w, nil, "Warning", "RequestNotServed", "Wake",
		"a request on Service %s was closed after %s: %s", rt.service, waited.Round(time.Second), reason)
}
