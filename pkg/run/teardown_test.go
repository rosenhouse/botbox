package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestTeardownRunsTheStepsInReverse(t *testing.T) {
	var order []string
	var down teardown
	for _, name := range []string{"cluster", "proxy", "target"} {
		down.push(name, func(context.Context) error {
			order = append(order, name)
			return nil
		})
	}

	if err := down.run(t.Context()); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}

	if want := []string{"target", "proxy", "cluster"}; !slices.Equal(order, want) {
		t.Errorf("The steps ran as %v, want %v.", order, want)
	}
}

func TestTeardownRunsEveryStepAndNamesWhatFailed(t *testing.T) {
	ran := 0
	var down teardown
	down.push("closing target.log", func(context.Context) error { ran++; return nil })
	down.push("stopping the proxy", func(context.Context) error { ran++; return errors.New("the listener is stuck") })
	down.push("stopping the target", func(context.Context) error { ran++; return nil })

	err := down.run(t.Context())

	if ran != 3 {
		t.Errorf("%d steps ran, want all 3: each holds a resource of its own.", ran)
	}
	if err == nil || !strings.Contains(err.Error(), "stopping the proxy: the listener is stuck") {
		t.Errorf("run returned %v, want the failing step named.", err)
	}
}

func TestTeardownRunsEachStepOnce(t *testing.T) {
	ran := 0
	var down teardown
	down.push("stopping the proxy", func(context.Context) error { ran++; return nil })

	_ = down.run(t.Context())
	_ = down.run(t.Context())

	if ran != 1 {
		t.Errorf("The step ran %d times, want once: a second Stop undoes nothing.", ran)
	}
}
