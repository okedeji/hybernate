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
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// pausedFrom is the copy of a pause record a paused target carries in
// v1alpha1.AnnotationPausedFrom: what's needed to undo the pause.
type pausedFrom struct {
	Replicas                   int32   `json:"replicas"`
	ScaledObject               string  `json:"scaledObject,omitempty"`
	ScaledObjectPausedReplicas *string `json:"scaledObjectPausedReplicas,omitempty"`
}

// RecordOn rebuilds the pause record a paused target carries, or returns
// nil when it carries none. The record has only what undoing the pause
// needs: when it was made, and what the workload ran then, are lost.
func RecordOn(target client.Object) (*v1alpha1.PauseStatus, error) {
	raw, ok := target.GetAnnotations()[v1alpha1.AnnotationPausedFrom]
	if !ok {
		return nil, nil
	}
	var from pausedFrom
	if err := json.Unmarshal([]byte(raw), &from); err != nil {
		return nil, fmt.Errorf("reading the %s annotation: %w", v1alpha1.AnnotationPausedFrom, err)
	}
	return &v1alpha1.PauseStatus{
		PreviousReplicas:           from.Replicas,
		ScaledObject:               from.ScaledObject,
		ScaledObjectPausedReplicas: from.ScaledObjectPausedReplicas,
	}, nil
}

// copyRecord writes the pause record onto the target, unless it's there
// already.
func copyRecord(ctx context.Context, c client.Client, target client.Object, pause *v1alpha1.PauseStatus) error {
	value, err := json.Marshal(pausedFrom{
		Replicas:                   pause.PreviousReplicas,
		ScaledObject:               pause.ScaledObject,
		ScaledObjectPausedReplicas: pause.ScaledObjectPausedReplicas,
	})
	if err != nil {
		return fmt.Errorf("encoding the pause record: %w", err)
	}
	if target.GetAnnotations()[v1alpha1.AnnotationPausedFrom] == string(value) {
		return nil
	}
	if err := annotate(ctx, c, target, string(value)); err != nil {
		return fmt.Errorf("copying the pause record to %s: %w", target.GetName(), err)
	}
	return nil
}

// dropRecord removes the target's copy of the pause record once the pause
// is undone. It's removed whatever the cache shows, since a copy the cache
// hasn't seen yet would be left behind, to be taken later for a pause
// that's long over.
func dropRecord(ctx context.Context, c client.Client, target client.Object) error {
	err := annotate(ctx, c, target, nil)
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("removing the pause record from %s: %w", target.GetName(), err)
}

// annotate sets AnnotationPausedFrom on the target, or removes it for nil.
func annotate(ctx context.Context, c client.Client, target client.Object, value any) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{v1alpha1.AnnotationPausedFrom: value}}})
	if err != nil {
		return fmt.Errorf("building the patch: %w", err)
	}
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, body), client.FieldOwner(v1alpha1.FieldManager))
}
