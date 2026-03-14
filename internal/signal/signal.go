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

package signal

import (
	"context"
	"fmt"
)

// Checker confirms whether a proposed action should proceed.
// Used by both idle detection and scaling to build signal consensus.
type Checker interface {
	Check(ctx context.Context, namespace, name string) (Result, error)
}

type Result struct {
	Confirm bool
	Reason  string
}

// CheckAll evaluates all signals and returns the first non-confirming result.
// If all signals confirm, it returns a confirming result. This implements
// the consensus model: every signal must agree before the caller acts.
func CheckAll(ctx context.Context, namespace, name string, signals []Checker) (Result, error) {
	for _, sig := range signals {
		res, err := sig.Check(ctx, namespace, name)
		if err != nil {
			return Result{}, fmt.Errorf("checking signal for %s/%s: %w", namespace, name, err)
		}
		if !res.Confirm {
			return Result{Confirm: false, Reason: fmt.Sprintf("signal denied: %s", res.Reason)}, nil
		}
	}
	return Result{Confirm: true, Reason: "all signals confirm"}, nil
}
