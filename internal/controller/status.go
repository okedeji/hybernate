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

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// statusFlushInterval bounds how often status is written when only
// continuously-moving values changed: cost totals and the activity clock. An
// awake workload is checked every minute, so writing on every check would
// cost one API write per workload per minute.
const statusFlushInterval = 5 * time.Minute

// persistStatus writes the workload's status if anything meaningful changed
// since it was last written, or if the continuously-moving values are due a
// flush. observed is the status as it was when the reconcile began.
func (r *Reconciler) persistStatus(ctx context.Context, workload *v1alpha1.ManagedWorkload, observed *v1alpha1.ManagedWorkloadStatus) error {
	if equality.Semantic.DeepEqual(withoutVolatile(observed), withoutVolatile(&workload.Status)) &&
		!flushDue(observed, r.now()) && !activityUnwritten(observed, &workload.Status, idleAfterFor(workload)) {
		return nil
	}
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

// withoutVolatile returns a copy of status without the values that change on
// every check, leaving what a reader needs to see promptly: phase,
// conditions, lifecycle records, and the forecast.
func withoutVolatile(status *v1alpha1.ManagedWorkloadStatus) *v1alpha1.ManagedWorkloadStatus {
	s := status.DeepCopy()
	s.Cost = nil
	if s.Activity != nil {
		s.Activity = &v1alpha1.ActivityStatus{TemplateHash: s.Activity.TemplateHash}
	}
	return s
}

// activityUnwritten reports whether the written status is further behind on
// the latest activity than a restart could afford: after one, what's written
// is all the operator knows, and a workload would pause up to that much
// early. The allowance is a small share of idleAfter, so a short idleAfter
// is written on every activity and a long one at the flush interval.
func activityUnwritten(observed, current *v1alpha1.ManagedWorkloadStatus, idleAfter time.Duration) bool {
	if current.Activity == nil {
		return false
	}
	if observed.Activity == nil {
		return true
	}
	behind := current.Activity.LastActivityTime.Sub(observed.Activity.LastActivityTime.Time)
	return behind > min(statusFlushInterval, idleAfter/10)
}

// flushDue reports whether the volatile values were last written at least
// statusFlushInterval ago, judged by the newest timestamp they carry.
func flushDue(observed *v1alpha1.ManagedWorkloadStatus, now time.Time) bool {
	var last time.Time
	if c := observed.Cost; c != nil && c.LastAccumulatedAt != nil {
		last = c.LastAccumulatedAt.Time
	}
	if a := observed.Activity; a != nil && a.LastEvaluatedTime != nil && a.LastEvaluatedTime.After(last) {
		last = a.LastEvaluatedTime.Time
	}
	return last.IsZero() || now.Sub(last) >= statusFlushInterval
}
