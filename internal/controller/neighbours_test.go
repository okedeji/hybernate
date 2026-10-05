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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func neighbour(namespace, name string, phase v1alpha1.WorkloadPhase, routed bool) *v1alpha1.ManagedWorkload {
	w := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "/" + name)},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
	if routed {
		w.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "shop", DoormanPort: 20001}}
	}
	return w
}

// When one workload behind a shared Service wakes, the paused ones routing
// that Service through the doorman are reconciled at once, so they hand it
// to the woken pods instead of at their next recheck.
func TestFindRoutedNeighbours(t *testing.T) {
	woken := neighbour("shop", "stable", v1alpha1.PhaseRunning, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		woken,
		neighbour("shop", "canary", v1alpha1.PhasePaused, true),
		neighbour("shop", "unrouted", v1alpha1.PhasePaused, false),
		neighbour("shop", "awake", v1alpha1.PhaseRunning, false),
		neighbour("blog", "elsewhere", v1alpha1.PhasePaused, true),
	).Build()
	r := &Reconciler{Client: c}

	got := r.findRoutedNeighbours(context.Background(), woken)

	require.Len(t, got, 1)
	assert.Equal(t, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "canary"}}, got[0])
}

func TestPhaseChanged(t *testing.T) {
	paused := neighbour("shop", "stable", v1alpha1.PhasePaused, true)
	resuming := neighbour("shop", "stable", v1alpha1.PhaseResuming, true)
	p := phaseChanged()

	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: paused, ObjectNew: resuming}), "a phase change")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: paused, ObjectNew: paused.DeepCopy()}),
		"a status write within the phase")
	assert.True(t, p.Delete(event.DeleteEvent{Object: paused}), "a workload going away")
	assert.False(t, p.Create(event.CreateEvent{Object: paused}), "a new workload routes nothing yet")
}
