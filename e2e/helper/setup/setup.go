// Package setup provides tiny composition helpers for the e2e setup steps:
// a named step type and a fail-fast sequencer. Each step takes a context (for
// cancellation) and returns an error; the Kubernetes clients it needs are
// carried on a kube.Client value owned by the spec and captured by each step's
// closure, not held as package state.
package setup

import (
	"context"
	"fmt"
)

// Func is a single setup step.
type Func func(ctx context.Context) error

// Named wraps fn so its errors are prefixed with name for clearer failures.
func Named(name string, fn Func) Func {
	return func(ctx context.Context) error {
		if err := fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
}

// AllFailFast runs steps sequentially, returning on the first error.
func AllFailFast(steps ...Func) Func {
	return func(ctx context.Context) error {
		for _, step := range steps {
			if err := step(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}
