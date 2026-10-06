//go:build reconciler_fuzzer

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestReconcilerFuzzer(t *testing.T) {
	fuzzer := exec.Command("bin/reconciler-fuzzer", "run", "--target", "targets/toy-widget/target.yaml",
		"--seed", "1", "--runs", "3")
	if deadline, ok := t.Deadline(); ok {
		// Stop reconciler-fuzzer before go test's -timeout, which would leave its
		// control plane running.
		left := time.Until(deadline).Truncate(time.Second) - 30*time.Second
		if left <= 0 {
			t.Fatal("go test's -timeout leaves reconciler-fuzzer no time")
		}
		fuzzer.Args = append(fuzzer.Args, "--deadline", left.String())
	}
	fuzzer.Dir = "../.." // launch.binary is relative to the repository root.
	fuzzer.Stdout, fuzzer.Stderr = os.Stdout, os.Stderr
	if err := fuzzer.Run(); err != nil {
		t.Fatalf("reconciler-fuzzer: %v", err)
	}
}
