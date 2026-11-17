// Package graceful provides a small, reusable lifecycle for command-line
// programs: a context cancelled on SIGINT/SIGTERM, tracked background
// goroutines, and a bounded shutdown window.
//
// SIGKILL and SIGSTOP are deliberately not handled — the OS does not deliver
// them to the process, so no cleanup can run for them. This package covers the
// signals that CAN be handled (Ctrl+C, `kill`, `docker stop`, systemd stop).
package graceful

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ErrShutdownTimeout is returned by Wait when background goroutines did not
// finish within the grace period after the context was cancelled.
var ErrShutdownTimeout = errors.New("graceful: shutdown timed out")

// Runner owns a cancellable context and the goroutines started under it, and
// guarantees that Wait does not return until they have all stopped (or the
// grace period elapses).
type Runner struct {
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// New returns a Runner whose context is cancelled on the first SIGINT/SIGTERM.
// Call Stop (usually deferred) to release the signal handler.
func New() *Runner {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return &Runner{ctx: ctx, stop: stop}
}

// Context is the lifecycle context; it is cancelled on a shutdown signal.
func (r *Runner) Context() context.Context { return r.ctx }

// Stop releases the signal handler and cancels the context. It is safe to call
// multiple times; defer it right after New.
func (r *Runner) Stop() { r.stop() }

// Go runs fn in a tracked goroutine. fn should observe r.Context() for
// cancellation and return promptly once it is done. Wait blocks on every
// goroutine started this way.
func (r *Runner) Go(fn func(ctx context.Context)) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn(r.ctx)
	}()
}

// Wait blocks until the context is cancelled (by signal or by the caller's own
// Stop), then waits up to grace for all tracked goroutines to finish. It
// returns nil on a clean stop, or ErrShutdownTimeout if the grace period
// elapsed first. A non-positive grace waits indefinitely.
func (r *Runner) Wait(grace time.Duration) error {
	<-r.ctx.Done()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	if grace <= 0 {
		<-done
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return ErrShutdownTimeout
	}
}
