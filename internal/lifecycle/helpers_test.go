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

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

type fakeScaler struct {
	replicas      int32
	readyReplicas int32
}

func (f *fakeScaler) GetScale(_ context.Context, _ client.Object) (*autoscalingv1.Scale, error) {
	return &autoscalingv1.Scale{
		Spec:   autoscalingv1.ScaleSpec{Replicas: f.replicas},
		Status: autoscalingv1.ScaleStatus{Replicas: f.readyReplicas},
	}, nil
}

func (f *fakeScaler) UpdateScale(_ context.Context, _ client.Object, scale *autoscalingv1.Scale) error {
	f.replicas = scale.Spec.Replicas
	return nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}
