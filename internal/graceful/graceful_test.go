package graceful

import (
	"context"
	"testing"
	"time"
)

func TestRunnerCleanStop(t *testing.T) {
	r := New()
	defer r.Stop()
	if r.Context() == nil {
		t.Fatal("Context() is nil")
	}

	ran := make(chan struct{})
	r.Go(func(ctx context.Context) {
		close(ran)
		<-ctx.Done() // return promptly on cancel
	})
	<-ran

	r.Stop() // cancels the context
	if err := r.Wait(time.Second); err != nil {
		t.Errorf("Wait after clean stop = %v, want nil", err)
	}
}

func TestRunnerWaitTimeout(t *testing.T) {
	r := New()
	defer r.Stop()

	block := make(chan struct{})
	r.Go(func(context.Context) {
		<-block // ignores cancellation → forces the grace window to elapse
	})
	r.Stop()
	if err := r.Wait(20 * time.Millisecond); err != ErrShutdownTimeout {
		t.Errorf("Wait = %v, want ErrShutdownTimeout", err)
	}
	close(block) // let the goroutine finish so the test leaks nothing
}

func TestRunnerWaitIndefinite(t *testing.T) {
	r := New()
	defer r.Stop()
	r.Go(func(ctx context.Context) { <-ctx.Done() })
	r.Stop()
	if err := r.Wait(0); err != nil { // non-positive grace waits indefinitely
		t.Errorf("Wait(0) = %v, want nil", err)
	}
}
