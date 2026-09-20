package launch

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStopGivesUpOnAProcessThatIsNeverReaped covers a target whose descendants
// hold its output open: the launcher reports it instead of blocking the run,
// and refuses to start a second instance behind it.
func TestStopGivesUpOnAProcessThatIsNeverReaped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 0.1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	binary := NewBinary(Options{Path: "/bin/sh", GracePeriod: 100 * time.Millisecond})
	binary.running = &process{cmd: cmd, done: make(chan struct{})} // nothing closes done
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := binary.Stop(ctx)

	if err == nil || !strings.Contains(err.Error(), "SIGKILL") {
		t.Fatalf("Stop reported %v for a process it could not reap.", err)
	}
	if err := binary.Start(t.Context(), "kubeconfig"); err == nil {
		t.Error("Start ran a second instance although the first was never reaped.")
	}
}

func TestRestartReportsAProcessItCannotReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 0.1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	binary := NewBinary(Options{Path: "/bin/sh", GracePeriod: 100 * time.Millisecond})
	binary.running = &process{cmd: cmd, done: make(chan struct{})} // nothing closes done

	if err := binary.Restart(t.Context()); err == nil {
		t.Error("Restart started a replacement although the old process was never reaped.")
	}
}
