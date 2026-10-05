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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
)

func realEngineRegistry() *engineRegistry {
	return newEngineRegistry(func() forecaster {
		return forecast.NewEngine(forecast.DefaultParams(), forecast.Settings{})
	})
}

// forecastReconciler runs automation with the real forecast engine against
// a fake API server, at a clock the test moves.
type forecastReconciler struct {
	*Reconciler
	now time.Time
}

func newForecastReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, c client.Client) *forecastReconciler {
	t.Helper()
	fr := &forecastReconciler{now: fixedTime}
	if c == nil {
		c = fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithStatusSubresource(&v1alpha1.ManagedWorkload{}).WithObjects(workload).Build()
	}
	fr.Reconciler = &Reconciler{
		Client:   c,
		Scheme:   testScheme(t),
		Recorder: events.NewFakeRecorder(50),
		pauser:   &stubPauser{},
		metrics:  &stubMetrics{cpuMillis: 250},
		engines:  realEngineRegistry(),
		clock:    func() time.Time { return fr.now },
	}
	return fr
}

// reconcile runs automation and writes status the way Reconcile does.
func (fr *forecastReconciler) reconcile(t *testing.T, workload *v1alpha1.ManagedWorkload) {
	t.Helper()
	require.NoError(t, fr.Get(context.Background(), client.ObjectKeyFromObject(workload), workload))
	observed := workload.Status.DeepCopy()
	_, err := fr.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.NoError(t, fr.persistStatus(context.Background(), workload, observed))
}

func forecastWorkload() *v1alpha1.ManagedWorkload {
	w := automationWorkload(v1alpha1.PhaseRunning)
	w.UID = "uid-1"
	// A recent activity evaluation, so only a meaningful change is written
	// before the next status flush.
	w.Status.Activity = &v1alpha1.ActivityStatus{LastEvaluatedTime: &metav1.Time{Time: fixedTime}}
	return w
}

// watch reconciles the workload once a minute until until, inclusive.
func (fr *forecastReconciler) watch(t *testing.T, workload *v1alpha1.ManagedWorkload, until time.Time) {
	t.Helper()
	for ; !fr.now.After(until); fr.now = fr.now.Add(time.Minute) {
		fr.reconcile(t, workload)
	}
	fr.now = until
}

func TestForecastState_LivesInStatusNotConfigMaps(t *testing.T) {
	workload := forecastWorkload()
	fr := newForecastReconciler(t, workload, nil)

	fr.watch(t, workload, fixedTime.Add(time.Hour))

	require.NotNil(t, workload.Status.Prediction)
	assert.NotEmpty(t, workload.Status.Prediction.State)
	restored, err := forecast.ImportEngine(workload.Status.Prediction.State, forecast.Settings{})
	require.NoError(t, err)
	assert.Equal(t, 1, restored.GetDataPoints())

	var cms corev1.ConfigMapList
	require.NoError(t, fr.List(context.Background(), &cms))
	assert.Empty(t, cms.Items, "the operator writes no ConfigMaps")
}

func TestForecastState_ChangesOnceAnHour(t *testing.T) {
	workload := forecastWorkload()
	fr := newForecastReconciler(t, workload, nil)
	fr.reconcile(t, workload)
	state := workload.Status.Prediction.State

	var changedAt []time.Time
	for fr.now = fixedTime.Add(time.Minute); !fr.now.After(fixedTime.Add(2 * time.Hour)); fr.now = fr.now.Add(time.Minute) {
		fr.reconcile(t, workload)
		if workload.Status.Prediction.State != state {
			state = workload.Status.Prediction.State
			changedAt = append(changedAt, fr.now)
		}
	}

	assert.Equal(t, []time.Time{fixedTime.Add(time.Hour), fixedTime.Add(2 * time.Hour)}, changedAt,
		"as each hour ends")
}

// TestForecastState_SurvivesARestart: a restarted operator, or a new leader,
// picks up from status. The hour the restart interrupted is skipped rather
// than learned from the part of it seen after.
func TestForecastState_SurvivesARestart(t *testing.T) {
	workload := forecastWorkload()
	fr := newForecastReconciler(t, workload, nil)
	fr.watch(t, workload, fixedTime.Add(2*time.Hour))

	restarted := newForecastReconciler(t, workload, fr.Client)
	restarted.now = fixedTime.Add(2*time.Hour + 30*time.Minute)
	restarted.watch(t, workload, fixedTime.Add(3*time.Hour))

	engine, err := restarted.engines.getOrCreate(workload.UID, forecast.Settings{}, "")
	require.NoError(t, err)
	assert.Equal(t, 2, engine.GetDataPoints(), "the hours before the restart are restored, and the one it interrupted skipped")
	assert.True(t, engine.Observed(fixedTime.Add(time.Hour)))
	assert.False(t, engine.Observed(fixedTime.Add(2*time.Hour)))

	restarted.watch(t, workload, fixedTime.Add(4*time.Hour))
	assert.Equal(t, 3, engine.GetDataPoints())
}

func TestForecastState_UnreadableStateStartsAfreshWithAWarning(t *testing.T) {
	gzipped := func(s string) string {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(s))
		_ = zw.Close()
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	for name, state := range map[string]string{
		"garbage":                        "not state",
		"an older version":               gzipped(`{"v":1}`),
		"a hand edit out of range":       gzipped(`{"v":2,"p":7}`),
		"a hand edit with broken arrays": gzipped(`{"v":2,"n":3,"lh":1,"ae":[1,2],"ay":[1]}`),
	} {
		t.Run(name, func(t *testing.T) {
			workload := forecastWorkload()
			workload.Status.Prediction = &v1alpha1.PredictionStatus{State: state}
			fr := newForecastReconciler(t, workload, nil)

			fr.reconcile(t, workload)

			restored, err := forecast.ImportEngine(workload.Status.Prediction.State, forecast.Settings{})
			require.NoError(t, err, "the bad state is replaced")
			assert.Zero(t, restored.GetDataPoints(), "the engine started afresh")
			assert.Contains(t, drainEvents(t, fr.Reconciler), "Warning "+ReasonForecastReset)
		})
	}
}

func TestEngineRegistry_AppliesSettingsEveryTime(t *testing.T) {
	engine := &stubForecaster{}
	reg := newEngineRegistry(func() forecaster { return engine })
	london, err := time.LoadLocation("Europe/London")
	require.NoError(t, err)

	_, err = reg.getOrCreate("uid", forecast.Settings{Threshold: 85}, "")
	require.NoError(t, err)
	_, err = reg.getOrCreate("uid", forecast.Settings{Threshold: 95, Location: london}, "")
	require.NoError(t, err)

	assert.Equal(t, forecast.Settings{Threshold: 95, Location: london}, engine.settings,
		"a changed spec.prediction.confidence applies to the engine already running")
}

func TestReconciler_AppliesTimezoneToTheForecast(t *testing.T) {
	workload := forecastWorkload()
	engine := &stubForecaster{phase: forecast.Observing}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	r.Timezone = tokyo
	r.engines.engines[workload.UID] = engine

	_, err = r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, tokyo, engine.settings.Location)
	assert.Equal(t, 85, engine.settings.Threshold)
}

func TestEngineRegistry_ForgetsADeletedWorkload(t *testing.T) {
	workload := lifecycleWorkload("forgotten", nil, v1alpha1.PhaseRunning)
	workload.UID = "uid-deleted"
	workload.Finalizers = []string{finalizerName}
	deleting := metav1.NewTime(fixedTime)
	workload.DeletionTimestamp = &deleting
	r := newTestReconciler(t, workload, &stubPauser{})
	old, err := r.engines.getOrCreate(workload.UID, forecast.Settings{}, "")
	require.NoError(t, err)

	_, err = r.Reconcile(context.Background(), reconcileFor("forgotten"))
	require.NoError(t, err)

	again, err := r.engines.getOrCreate(workload.UID, forecast.Settings{}, "")
	require.NoError(t, err)
	assert.NotSame(t, old, again, "a deleted workload's engine is dropped")
}

func TestForecast_RejectedObservationDoesNotBlockAutomation(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing, observeErr: forecast.ErrInvalidObservation}
	metrics := &stubMetrics{}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics})

	lookEveryMinute(t, r, workload, metrics, fixedTime, fixedTime.Add(time.Hour), func(time.Time) float64 { return 10 })

	assert.Zero(t, engine.observeCalls)
	assert.NotNil(t, workload.Status.Prediction)
}

func TestForecast_ExportFailureKeepsTheLastSavedState(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Status.Prediction = &v1alpha1.PredictionStatus{State: "saved"}
	engine := &stubForecaster{phase: forecast.DailyActive, exportErr: errors.New("boom")}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, "saved", workload.Status.Prediction.State)
	assert.Equal(t, "Active", workload.Status.Prediction.DailyPhase)
}
