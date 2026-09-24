// Command botbox exercises a Kubernetes controller against the generic
// invariants of DESIGN.md §6.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	c := &cli{stdout: os.Stdout, stderr: os.Stderr, open: openSession, newGenerator: rapidGenerator}
	os.Exit(c.interruptible(os.Args[1:]))
}

// interruptible runs the invocation until SIGINT, SIGTERM or SIGHUP interrupts
// it. botbox then stops what it started and dies of that signal, as a shell
// expects. A second signal kills it at once. A SIGHUP or SIGINT that botbox
// started out ignoring, as under nohup, stays ignored.
func (c *cli) interruptible(args []string) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			signal.Notify(signals, sig)
		}
	}
	go func() {
		sig := <-signals
		// The signal may have killed whatever reads botbox's output.
		signal.Ignore(syscall.SIGPIPE)
		signal.Stop(signals)
		fmt.Fprintln(c.stderr, "botbox: an interrupt arrived, so botbox is stopping what it started. Interrupt again to exit now and leave it running.")
		cancel(interrupt{sig.(syscall.Signal)})
	}()

	code := c.main(ctx, args)
	if stop, ok := interruption(ctx); ok {
		stop.die()
	}
	return code
}

// interrupt is the cause of an invocation a signal ended.
type interrupt struct{ signal syscall.Signal }

func (i interrupt) Error() string { return i.signal.String() }

// interruption is the interrupt that ended ctx, if one did.
func interruption(ctx context.Context) (interrupt, bool) {
	var i interrupt
	return i, errors.As(context.Cause(ctx), &i)
}

// die kills botbox with the signal, which no handler catches any longer. Another
// thread may take the signal, so die waits for it rather than return.
func (i interrupt) die() {
	if syscall.Kill(os.Getpid(), i.signal) == nil {
		time.Sleep(time.Second)
	}
}
