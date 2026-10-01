//go:build botbox

package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestBotbox(t *testing.T) {
	botbox := exec.Command("botbox", "run", "--target", "targets/toy-widget/target.yaml",
		"--seed", "1", "--runs", "3", "--deadline", "5m")
	botbox.Dir = "../.." // launch.binary is relative to the repository root.
	botbox.Stdout, botbox.Stderr = os.Stdout, os.Stderr
	if err := botbox.Run(); err != nil {
		t.Fatalf("botbox: %v", err)
	}
}
