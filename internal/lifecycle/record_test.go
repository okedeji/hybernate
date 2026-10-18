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

package lifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

// The target carries a copy of the pause record from before it's scaled to
// zero until it's handed back, so a pause whose ManagedWorkload status is
// lost can still be undone exactly.
func TestPause_CopiesTheRecordToTheTargetUntilHandedBack(t *testing.T) {
	tests := []struct {
		name string
		undo func(t *testing.T, p *Pauser, c client.Client, w *v1alpha1.ManagedWorkload)
	}{
		{name: "resumed", undo: func(t *testing.T, p *Pauser, c client.Client, w *v1alpha1.ManagedWorkload) {
			_, err := resume(p, w)
			require.NoError(t, err)
			settle(t, c, 3)
			settled, err := resume(p, w)
			require.NoError(t, err)
			require.True(t, settled)
		}},
		{name: "restored", undo: func(t *testing.T, p *Pauser, _ client.Client, w *v1alpha1.ManagedWorkload) {
			_, err := p.Restore(context.Background(), w)
			require.NoError(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			so := scaledObjectFor(0)
			so.SetAnnotations(map[string]string{autoscaler.PausedReplicasAnnotation: "5"})
			c := newClient(t, deployment(3, 0), so)
			p := newTestPauser(c)
			workload := apiWorkload()
			require.NoError(t, p.Prepare(context.Background(), workload))

			_, err := p.Pause(context.Background(), workload)
			require.NoError(t, err)

			got, err := RecordOn(apiTarget(t, c))
			require.NoError(t, err)
			assert.Equal(t, &v1alpha1.PauseStatus{PreviousReplicas: 3, ScaledObject: scaledObjectName,
				ScaledObjectPausedReplicas: ptr.To("5")}, got)

			tt.undo(t, p, c, workload)

			got, err = RecordOn(apiTarget(t, c))
			require.NoError(t, err)
			assert.Nil(t, got, "handed back, it carries no pause")
			v, _ := pausedAnnotation(t, c)
			assert.Equal(t, "5", v, "the ScaledObject's own value is back")
		})
	}
}

func TestRecordOn_Unreadable(t *testing.T) {
	d := deployment(0, 0)
	d.Annotations = map[string]string{v1alpha1.AnnotationPausedFrom: "3"}

	_, err := RecordOn(d)

	assert.ErrorContains(t, err, v1alpha1.AnnotationPausedFrom)
}

func apiTarget(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), apiKey, &d))
	return &d
}
