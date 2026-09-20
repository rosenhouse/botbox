package run

import (
	"context"
	"errors"
	"fmt"
)

// teardown holds what Start brought up, in the order it did.
type teardown struct{ steps []step }

type step struct {
	what string
	undo func(context.Context) error
}

func (t *teardown) push(what string, undo func(context.Context) error) {
	t.steps = append(t.steps, step{what: what, undo: undo})
}

// run undoes every step, the last one first. A step that fails does not stop
// the ones below it, because each holds a resource of its own. Every step runs
// once, so a second Stop undoes nothing.
func (t *teardown) run(ctx context.Context) error {
	steps := t.steps
	t.steps = nil
	var failures []error
	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i].undo(ctx); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", steps[i].what, err))
		}
	}
	return errors.Join(failures...)
}
