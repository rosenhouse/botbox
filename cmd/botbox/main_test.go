package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse/botbox/pkg/run"
	"github.com/rosenhouse/botbox/pkg/target"
)

// asBotbox has the test binary run as botbox, over a session that runs until
// it is interrupted, so that a test signals a process other than its own.
const asBotbox = "BOTBOX_TEST_AS_BOTBOX"

// closedFile is where that botbox records that it closed its session, and
// closeTakes how long closing takes.
const (
	closedFile = "BOTBOX_TEST_CLOSED_FILE"
	closeTakes = "BOTBOX_TEST_CLOSE_TAKES"
)

func TestMain(m *testing.M) {
	if os.Getenv(asBotbox) != "" {
		c := &cli{stdout: os.Stdout, stderr: os.Stderr, newGenerator: countingGenerator(nil),
			open: func(options, *target.Target) (session, error) { return untilInterrupted{}, nil }}
		os.Exit(c.interruptible(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type untilInterrupted struct{}

func (untilInterrupted) vet(*target.Target) error { return nil }

func (untilInterrupted) execute(ctx context.Context, _ *target.Target, _ run.Sequence, dir string, _ run.Checker) (run.Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return run.Result{}, err
	}
	<-ctx.Done()
	return run.Result{}, ctx.Err()
}

func (untilInterrupted) close() error {
	takes, _ := time.ParseDuration(os.Getenv(closeTakes))
	time.Sleep(takes)
	return os.WriteFile(os.Getenv(closedFile), nil, 0o644)
}

// botboxProcess is the test binary running as botbox, in a process group of
// its own.
type botboxProcess struct {
	cmd            *exec.Cmd
	exited         <-chan struct{}
	closed         string
	out            string
	stdout, stderr *output
	lines          chan string
	// readers are the ends of botbox's output pipes that the test reads.
	readers []*os.File
	reading sync.WaitGroup
}

// output keeps what botbox wrote to one stream, and passes each line on.
type output struct {
	mu      sync.Mutex
	written strings.Builder
	partial string
	lines   chan<- string
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.written.Write(p)
	o.partial += string(p)
	for {
		line, rest, found := strings.Cut(o.partial, "\n")
		if !found {
			return len(p), nil
		}
		o.lines <- line
		o.partial = rest
	}
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.written.String()
}

// startBotbox starts botbox under the shell's prefix, if any, which can set
// how it starts out handling signals.
func startBotbox(t *testing.T, takes time.Duration, prefix ...string) *botboxProcess {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1000)
	b := &botboxProcess{closed: filepath.Join(t.TempDir(), "closed"), out: t.TempDir(),
		stdout: &output{lines: lines}, stderr: &output{lines: lines}, lines: lines}
	args := append(prefix, self, "run", "--target", toyTargetYAML, "--out", b.out, "--runs", "3")
	b.cmd = exec.Command(args[0], args[1:]...)
	b.cmd.Env = append(os.Environ(), asBotbox+"=1", closedFile+"="+b.closed, closeTakes+"="+takes.String())
	b.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, stderr := b.pipe(t, b.stdout), b.pipe(t, b.stderr)
	b.cmd.Stdout, b.cmd.Stderr = stdout, stderr
	err = b.cmd.Start()
	_, _ = stdout.Close(), stderr.Close()
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = b.cmd.Wait()
		b.reading.Wait()
	}()
	b.exited = exited
	t.Cleanup(func() {
		_ = b.cmd.Process.Kill()
		<-exited
	})
	return b
}

// pipe passes what botbox writes to one stream on to out.
func (b *botboxProcess) pipe(t *testing.T, out *output) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b.readers = append(b.readers, reader)
	b.reading.Go(func() { _, _ = io.Copy(out, reader) })
	return writer
}

// stopReading closes the pipes botbox writes to, as a Ctrl-C does to the tee
// that reads them.
func (b *botboxProcess) stopReading() {
	for _, reader := range b.readers {
		_ = reader.Close()
	}
}

// waitFor waits for botbox to print a line holding want.
func (b *botboxProcess) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		select {
		case line := <-b.lines:
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("botbox never printed %q.\nstdout:\n%s\nstderr:\n%s", want, b.stdout, b.stderr)
		}
	}
}

// signal sends sig to botbox, or to its process group as a terminal's Ctrl-C
// does.
func (b *botboxProcess) signal(t *testing.T, sig syscall.Signal, group bool) {
	t.Helper()
	pid := b.cmd.Process.Pid
	if group {
		pid = -pid
	}
	if err := syscall.Kill(pid, sig); err != nil {
		t.Fatal(err)
	}
}

// exit waits for botbox to exit, and returns how it did.
func (b *botboxProcess) exit(t *testing.T, within time.Duration) syscall.WaitStatus {
	t.Helper()
	select {
	case <-b.exited:
	case <-time.After(within):
		t.Fatalf("botbox was still running %v after the signal.\nstderr:\n%s", within, b.stderr)
	}
	return b.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

func (b *botboxProcess) closedItsSession() bool {
	_, err := os.Stat(b.closed)
	return err == nil
}

// A signal interrupts botbox, which stops what it started and then dies of
// that signal, so that a shell, make or timeout sees what happened.
func TestASignalStopsBotboxAndThenKillsIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
		group  bool
	}{
		{"SIGTERM", syscall.SIGTERM, false},
		{"a terminal's Ctrl-C", syscall.SIGINT, true},
		{"SIGHUP", syscall.SIGHUP, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if signal.Ignored(test.signal) {
				t.Skipf("The tests run ignoring %v, and so would botbox.", test.signal)
			}
			b := startBotbox(t, 0)
			b.waitFor(t, "run 1:")

			b.signal(t, test.signal, test.group)

			status := b.exit(t, 10*time.Second)
			if !status.Signaled() || status.Signal() != test.signal {
				t.Errorf("botbox ended with %v, want death by %v.", status, test.signal)
			}
			if !b.closedItsSession() {
				t.Error("botbox never closed its session.")
			}
			stderr := b.stderr.String()
			for _, want := range []string{"botbox: an interrupt arrived", "run 1: an interrupt stopped the run"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("botbox printed\n%s\nwhich does not say %q.", stderr, want)
				}
			}
			written := readSummary(t, b.out)
			if written.Outcome != "interrupted" || written.ExitCode != 128+int(test.signal) || written.Runs[0].Outcome != "interrupted" {
				t.Errorf("The summary says %s, exit %d, and run 1 %s, want run 1 interrupted and exit %d.",
					written.Outcome, written.ExitCode, written.Runs[0].Outcome, 128+int(test.signal))
			}
		})
	}
}

// botbox 2>&1 | tee log: an interrupt kills tee too, and botbox writes on.
// SIGTERM stands in for a Ctrl-C, which a background job starts out ignoring.
func TestAnInterruptOutlivesTheReaderOfItsOutput(t *testing.T) {
	b := startBotbox(t, 0)
	b.waitFor(t, "run 1:")
	b.stopReading()

	b.signal(t, syscall.SIGTERM, true)

	status := b.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("botbox ended with %v, want death by SIGTERM.", status)
	}
	if !b.closedItsSession() {
		t.Error("botbox never closed its session.")
	}
}

func TestASecondSignalKillsBotboxAtOnce(t *testing.T) {
	b := startBotbox(t, time.Hour)
	b.waitFor(t, "run 1:")
	b.signal(t, syscall.SIGTERM, false)
	b.waitFor(t, "botbox: an interrupt arrived")

	b.signal(t, syscall.SIGTERM, false)

	status := b.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("botbox ended with %v, want death by SIGTERM.", status)
	}
	if b.closedItsSession() {
		t.Error("botbox closed its session, and the second signal came first.")
	}
}

// nohup starts a program ignoring SIGHUP, and a shell starts a background job
// ignoring SIGINT. botbox leaves those alone.
func TestASignalBotboxWasStartedIgnoringStaysIgnored(t *testing.T) {
	b := startBotbox(t, 0, "/bin/sh", "-c", `trap "" HUP INT; exec "$@"`, "sh")
	b.waitFor(t, "run 1:")

	b.signal(t, syscall.SIGHUP, false)
	b.signal(t, syscall.SIGINT, false)
	b.signal(t, syscall.SIGTERM, false)

	status := b.exit(t, 10*time.Second)
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Errorf("botbox ended with %v, want death by SIGTERM: it was started ignoring SIGHUP and SIGINT.", status)
	}
}
