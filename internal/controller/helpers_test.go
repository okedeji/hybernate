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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func TestSavePredictionState_WritesOnlyWhenStateChanges(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: "aaa"},
	}
	scheme := testScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(workload).Build(),
		Scheme: scheme,
	}
	resourceVersion := func() string {
		var cm corev1.ConfigMap
		require.NoError(t, r.Get(context.Background(),
			client.ObjectKey{Namespace: "default", Name: predictionConfigMapName("api")}, &cm))
		return cm.ResourceVersion
	}

	r.savePredictionState(context.Background(), workload, `{"v":1}`)
	created := resourceVersion()

	r.savePredictionState(context.Background(), workload, `{"v":1}`)
	assert.Equal(t, created, resourceVersion(), "an unchanged state must not be written again")

	r.savePredictionState(context.Background(), workload, `{"v":2}`)
	assert.NotEqual(t, created, resourceVersion())
}
