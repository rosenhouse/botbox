//go:build botbox

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestBotbox(t *testing.T) {
	botbox := exec.Command("bin/botbox", "run", "--target", "targets/toy-widget/target.yaml",
		"--seed", "1", "--runs", "3")
	if deadline, ok := t.Deadline(); ok {
		// Stop botbox before go test's -timeout, which would leave its control plane running.
		left := time.Until(deadline).Truncate(time.Second) - 30*time.Second
		if left <= 0 {
			t.Fatal("go test's -timeout leaves botbox no time")
		}
		botbox.Args = append(botbox.Args, "--deadline", left.String())
	}
	botbox.Dir = "../.." // launch.binary is relative to the repository root.
	botbox.Stdout, botbox.Stderr = os.Stdout, os.Stderr
	if err := botbox.Run(); err != nil {
		t.Fatalf("botbox: %v", err)
	}
}
