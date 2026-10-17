// Package sigctx turns SIGINT/SIGTERM into context cancellation for the
// long-running service commands (D7).
package sigctx

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// signalCause is the context cause recorded when a stop signal arrives.
type signalCause string

func (c signalCause) Error() string { return string(c) + " received" }

// NotifyContext returns a context cancelled by the first SIGINT or SIGTERM,
// with a cause naming the signal (see Reason). Right after the first signal
// the default signal handling is restored, so a second signal during the
// shutdown drain terminates the process immediately. stop releases the
// signal handler and cancels the context.
func NotifyContext(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-ch:
			signal.Stop(ch)
			name := "SIGTERM"
			if sig == os.Interrupt {
				name = "SIGINT"
			}
			cancel(signalCause(name))
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel(nil)
	}
}

// Reason names the signal that cancelled ctx ("SIGINT" or "SIGTERM"), or
// returns "context canceled" for any other cancellation.
func Reason(ctx context.Context) string {
	if sc, ok := errors.AsType[signalCause](context.Cause(ctx)); ok {
		return string(sc)
	}
	return "context canceled"
}
