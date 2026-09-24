package launch

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
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

// The restarts after the first wait twice as long each time, up to a cap, as a
// kubelet's do.
func TestTheSupervisorBacksOffAsAKubeletDoes(t *testing.T) {
	for _, delay := range []struct {
		backoff  time.Duration
		restarts int
		want     time.Duration
	}{
		{time.Second, 0, 0},
		{time.Second, 1, time.Second},
		{time.Second, 2, 2 * time.Second},
		{time.Second, 3, 4 * time.Second},
		{time.Second, 9, 256 * time.Second},
		{time.Second, 10, MaxBackoff},
		{time.Second, 100, MaxBackoff},
		{0, 1, DefaultBackoff},
		{0, 2, 2 * DefaultBackoff},
	} {
		binary := NewBinary(Options{Backoff: delay.backoff})
		if got := binary.backoff(delay.restarts); got != delay.want {
			t.Errorf("With a backoff of %v, restart %d waits %v, want %v.", delay.backoff, delay.restarts+1, got, delay.want)
		}
	}
	if DefaultBackoff != 10*time.Second || MaxBackoff != 5*time.Minute {
		t.Errorf("The backoff runs from %v to %v, want a kubelet's 10s to 5m.", DefaultBackoff, MaxBackoff)
	}
}

// A target can exit between the wait that converged and the call that
// supervises it. That exit came after convergence too.
func TestSuperviseRestartsATargetThatAlreadyExited(t *testing.T) {
	binary := NewBinary(Options{Path: "/bin/sh", Args: []string{"-c", "exit 3"}, Backoff: time.Hour})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	if err := binary.Start(t.Context(), "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !binary.reaped(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("The launcher never handled the exit.")
		}
	}
	heard := make(chan error, 2)

	binary.Supervise(func(exit error, _ time.Time) { heard <- exit })

	// The restarted target exits too, so a second exit shows the restart.
	for _, which := range []string{"the exit that came before it", "an exit of the restarted target"} {
		select {
		case exit := <-heard:
			if exit == nil || !strings.Contains(exit.Error(), "exit status 3") {
				t.Errorf("The supervisor reported %v for %s, want exit status 3.", exit, which)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("The supervisor never reported %s.", which)
		}
	}
}

// Stop ends supervision even where it cannot reap the process, so that the
// process exiting later starts nothing.
func TestAStopThatGivesUpEndsSupervision(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 0.1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	binary := NewBinary(Options{Path: "/bin/sh", Args: []string{"-c", "echo restarted"}, GracePeriod: 100 * time.Millisecond})
	unreaped := &process{cmd: cmd, done: make(chan struct{})}
	binary.running = unreaped
	var heard atomic.Bool
	binary.Supervise(func(error, time.Time) { heard.Store(true) })
	if err := binary.Stop(t.Context()); err == nil {
		t.Fatal("Stop reaped a process whose done never closes.")
	}

	close(unreaped.done)
	binary.reap(unreaped)
	time.Sleep(100 * time.Millisecond)

	binary.mu.Lock()
	defer binary.mu.Unlock()
	if heard.Load() || binary.running != unreaped {
		t.Errorf("A process reaped after Stop was restarted.")
	}
}

// reaped reports whether the launcher has handled the exit of the process it
// holds.
func (b *Binary) reaped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running != nil && b.running.reaped
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
