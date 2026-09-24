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
	// Exited is closed once the target has stopped and will not start again,
	// so that a caller waiting on the target ends where it does.
	Exited() <-chan struct{}
	// Supervise restarts the target whenever it exits on its own, as a
	// kubelet restarts a container, until ctx ends. It tells onExit why the
	// target stopped and when it starts again.
	Supervise(ctx context.Context, onExit func(exit error, restart time.Time))
}

// Status is what the launcher knows of the target process. It is the process's
// status, not a health probe (DESIGN.md §5.1).
type Status struct {
	// Running is whether the target process is alive, or will be once the
	// supervisor has restarted it.
	Running bool
	// Restarting is whether a supervised target has exited and waits to start
	// again.
	Restarting bool
	// Started is when the process botbox holds started.
	Started time.Time
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

// A supervised target restarts at once the first time. Each later restart
// waits twice as long as the one before, from DefaultBackoff up to MaxBackoff.
const (
	DefaultBackoff = 10 * time.Second
	MaxBackoff     = 5 * time.Minute
)

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
	// Backoff is how long a supervised target waits for its second restart.
	// Zero means DefaultBackoff.
	Backoff time.Duration
}

// Binary runs a target as a local process.
type Binary struct {
	options Options

	mu         sync.Mutex
	running    *process
	kubeconfig string
	// onExit hears each exit of a supervised target. It is nil until
	// Supervise, and supervision ends with supervising.
	onExit      func(error, time.Time)
	supervising context.Context
	restarts    int
	// gone closes once a supervised target will not start again, and failed
	// says why.
	gone   chan struct{}
	failed error
}

type process struct {
	cmd     *exec.Cmd
	started time.Time
	// done closes once the process has exited and been reaped, after exit is
	// set.
	done chan struct{}
	exit error
	// reaped is set once the launcher has seen the exit, restarting the
	// process if it was supervised.
	reaped bool
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
	running := &process{cmd: cmd, started: time.Now(), done: make(chan struct{})}
	go func() {
		if running.exit = cmd.Wait(); running.exit == nil {
			running.exit = ErrExitedZero
		}
		close(running.done)
		b.reap(running)
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
// is not.
func (b *Binary) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.running == nil:
		return Status{}
	case b.failed != nil:
		return Status{Exit: b.failed}
	}
	running := Status{Running: true, Started: b.running.started}
	select {
	case <-b.running.done:
		if b.onExit == nil {
			return Status{Exit: b.running.exit}
		}
		running.Restarting = true
	default:
	}
	return running
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
	switch {
	case b.running == nil:
		return noTarget
	case b.onExit != nil:
		return b.gone
	}
	return b.running.done
}

// Supervise restarts the target whenever it exits on its own, until ctx ends:
// at once the first time, and after the backoff every later time. A target that
// exited before the call restarts now.
func (b *Binary) Supervise(ctx context.Context, onExit func(exit error, restart time.Time)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onExit, b.supervising, b.gone = onExit, ctx, make(chan struct{})
	if b.running != nil && b.running.reaped {
		b.restartLater(b.running)
	}
}

// reap sees a process that has exited. Stop and Restart replace the process
// they end, so only an exit of the target's own restarts it.
func (b *Binary) reap(exited *process) {
	b.mu.Lock()
	defer b.mu.Unlock()
	exited.reaped = true
	if b.onExit != nil && b.running == exited {
		b.restartLater(exited)
	}
}

// restartLater tells the supervisor why the target stopped and when it starts
// again, then starts it once the backoff has passed. The caller holds b.mu, so
// that Stop and Restart wait until the exit is heard.
func (b *Binary) restartLater(exited *process) {
	if b.supervising.Err() != nil {
		b.stayDown(exited.exit)
		return
	}
	delay := b.backoff(b.restarts)
	b.restarts++
	b.onExit(exited.exit, time.Now().Add(delay))
	time.AfterFunc(delay, func() { b.restart(exited) })
}

// restart starts the target again, unless Stop or Restart already replaced the
// process that exited or supervision has ended.
func (b *Binary) restart(exited *process) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.running != exited:
	case b.supervising.Err() != nil:
		b.stayDown(exited.exit)
	default:
		if err := b.start(b.kubeconfig); err != nil {
			b.stayDown(fmt.Errorf("%w, and restarting it failed: %w", exited.exit, err))
		}
	}
}

func (b *Binary) stayDown(why error) {
	b.failed = why
	close(b.gone)
}

// backoff is how long the restart that follows the given number of restarts
// waits.
func (b *Binary) backoff(restarts int) time.Duration {
	if restarts == 0 {
		return 0
	}
	delay := b.options.Backoff
	if delay <= 0 {
		delay = DefaultBackoff
	}
	for range restarts - 1 {
		if delay >= MaxBackoff {
			break
		}
		delay *= 2
	}
	return min(delay, MaxBackoff)
}

// Stop sends SIGTERM and escalates to SIGKILL once the grace period or ctx
// expires. Stopping a target that is not running does nothing. A stopped
// target is supervised no longer.
func (b *Binary) Stop(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onExit = nil
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
