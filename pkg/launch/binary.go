// Package launch starts, stops and restarts the target process
// (DESIGN.md §5.1). botbox does not probe the target for health.
package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Launcher runs one target.
type Launcher interface {
	// Start runs the target against a kubeconfig that points at the proxy.
	Start(ctx context.Context, kubeconfig string) error
	// Stop shuts the target down: SIGTERM, then SIGKILL after a grace period.
	Stop(ctx context.Context) error
	// Restart crashes the target: SIGKILL, then Start once it is reaped.
	Restart(ctx context.Context) error
	// Status reports whether the target is still running.
	Status() Status
	// Exited is closed once the running target has stopped, so that a caller
	// waiting on the target ends where it does.
	Exited() <-chan struct{}
}

// Status is what the launcher knows of the target process. It is the process's
// status, not a health probe (DESIGN.md §5.1).
type Status struct {
	// Running is whether the target process is alive.
	Running bool
	// Exit is why a target that ran stopped: an *exec.ExitError naming its exit
	// status or signal, or ErrExitedZero. It is nil while the target runs, and
	// before Start and after Stop, when botbox is running no target.
	Exit error
}

// ErrExitedZero is the Exit of a target that ended successfully, which os/exec
// reports as no error at all.
var ErrExitedZero = errors.New("exit status 0")

// DefaultGracePeriod is how long Stop waits after SIGTERM before it escalates.
const DefaultGracePeriod = 5 * time.Second

// Options configure a Binary.
type Options struct {
	// Path is the binary to exec, relative to the working directory.
	Path string
	// Args are its arguments. Start substitutes $KUBECONFIG and $NAMESPACE in
	// them and in the values of Env.
	Args []string
	// Namespace is the run namespace.
	Namespace string
	// Env overrides variables the target inherits.
	Env map[string]string
	// Log receives the target's stdout and stderr, as target.log.
	Log io.Writer
	// GracePeriod is how long Stop waits after SIGTERM. Zero means
	// DefaultGracePeriod.
	GracePeriod time.Duration
}

// Binary runs a target as a local process.
type Binary struct {
	options Options

	mu         sync.Mutex
	running    *process
	kubeconfig string
}

type process struct {
	cmd *exec.Cmd
	// done closes once the process has exited and been reaped, after exit is
	// set.
	done chan struct{}
	exit error
}

var _ Launcher = (*Binary)(nil)

func NewBinary(options Options) *Binary { return &Binary{options: options} }

// Start execs the binary with KUBECONFIG set to kubeconfig. The context bounds
// the call, not the life of the process.
func (b *Binary) Start(ctx context.Context, kubeconfig string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running != nil {
		return fmt.Errorf("%s is already running", b.options.Path)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.start(kubeconfig)
}

func (b *Binary) start(kubeconfig string) error {
	placeholders := strings.NewReplacer("$KUBECONFIG", kubeconfig, "$NAMESPACE", b.options.Namespace)
	args := make([]string, len(b.options.Args))
	for i, arg := range b.options.Args {
		args[i] = placeholders.Replace(arg)
	}
	cmd := exec.Command(b.options.Path, args...)
	cmd.Env = b.environment(placeholders, kubeconfig)
	log := b.options.Log
	if log == nil {
		log = io.Discard
	}
	// One writer for both streams, so that os/exec gives the target a single
	// pipe and the log holds no interleaved writes. The pipe is also what makes
	// Wait, and so Restart, outlast a descendant that inherited the target's
	// output.
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", b.options.Path, err)
	}
	running := &process{cmd: cmd, done: make(chan struct{})}
	go func() {
		if running.exit = cmd.Wait(); running.exit == nil {
			running.exit = ErrExitedZero
		}
		close(running.done)
	}()
	b.running = running
	b.kubeconfig = kubeconfig
	return nil
}

// environment is botbox's own, with Env and then KUBECONFIG over it. os/exec
// keeps the last value of a repeated name.
func (b *Binary) environment(placeholders *strings.Replacer, kubeconfig string) []string {
	env := os.Environ()
	for _, name := range slices.Sorted(maps.Keys(b.options.Env)) {
		env = append(env, name+"="+placeholders.Replace(b.options.Env[name]))
	}
	return append(env, "KUBECONFIG="+kubeconfig)
}

// Status reports whether the target is still running, and why it stopped if it
// is not. A target that stopped on its own took the run with it, which is a
// harness error rather than a finding against the target (DESIGN.md §11).
func (b *Binary) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		return Status{}
	}
	select {
	case <-b.running.done:
		return Status{Exit: b.running.exit}
	default:
		return Status{Running: true}
	}
}

// noTarget is the Exited of a launcher with nothing left to wait for.
var noTarget = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Exited answers a closed channel while no target runs, as Status answers that
// none is running.
func (b *Binary) Exited() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		return noTarget
	}
	return b.running.done
}

// Stop sends SIGTERM and escalates to SIGKILL once the grace period or ctx
// expires. Stopping a target that is not running does nothing.
func (b *Binary) Stop(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		return nil
	}
	if err := b.terminate(ctx, b.running); err != nil {
		return err
	}
	b.running = nil
	return nil
}

func (b *Binary) terminate(ctx context.Context, running *process) error {
	if err := signal(running, syscall.SIGTERM); err != nil {
		return err
	}
	grace := time.NewTimer(b.gracePeriod())
	defer grace.Stop()
	select {
	case <-running.done:
		return nil
	case <-grace.C:
	case <-ctx.Done():
	}
	return b.kill(running)
}

// Restart kills the target and execs it again once it has been reaped, so that
// a fixed port or lock file is released first (DESIGN.md §5.1).
func (b *Binary) Restart(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		return fmt.Errorf("%s is not running", b.options.Path)
	}
	if err := b.kill(b.running); err != nil {
		return err
	}
	b.running = nil
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.start(b.kubeconfig)
}

func (b *Binary) gracePeriod() time.Duration {
	if b.options.GracePeriod <= 0 {
		return DefaultGracePeriod
	}
	return b.options.GracePeriod
}

// kill sends SIGKILL and waits for the process to be reaped, so that a fixed
// port or lock file is free before another instance starts. A descendant that
// inherited the target's output keeps the process unreaped, so the wait gives
// up after the grace period, leaving the target recorded as running.
func (b *Binary) kill(running *process) error {
	if err := signal(running, syscall.SIGKILL); err != nil {
		return err
	}
	reap := time.NewTimer(b.gracePeriod())
	defer reap.Stop()
	select {
	case <-running.done:
		return nil
	case <-reap.C:
		return fmt.Errorf("%s did not exit within %v of SIGKILL", running.cmd.Path, b.gracePeriod())
	}
}

// signal delivers sig, tolerating a process that has already exited.
func signal(running *process, sig syscall.Signal) error {
	err := running.cmd.Process.Signal(sig)
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return fmt.Errorf("sending %s to %s: %w", sig, running.cmd.Path, err)
}
