// Package cronloop runs a task on a cron schedule: the first tick starts
// immediately, following slots are computed from the previous slot (no
// drift), a slot that arrives while the task is still running is skipped and
// reported as an overrun, slots already in the past (after a wall-clock jump
// or suspend) are reported as one overrun and skipped, and cancelling the
// context stops the loop at once (D7) after waiting for the active task.
package cronloop

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a1ekseev/scd/internal/common/cron"
)

// Hooks receives loop notifications. Nil hooks are ignored. OnError runs on
// the task goroutine and OnOverrun on the loop goroutine, so they may be
// called concurrently.
type Hooks struct {
	// OnError receives a non-nil error returned by the task.
	OnError func(error)
	// OnOverrun is called with the slot skipped because the task was busy.
	OnOverrun func(scheduledAt time.Time)
}

// Run executes task on schedule s until ctx is cancelled, then waits for the
// active task to return. The task receives ctx; honouring cancellation is the
// task's responsibility. If s.Next returns the zero time no further ticks are
// scheduled and Run waits for ctx to be done.
func Run(ctx context.Context, s cron.Schedule, task func(context.Context) error, h Hooks) {
	var (
		wg      sync.WaitGroup
		running atomic.Bool
	)
	defer wg.Wait()
	start := func() {
		running.Store(true)
		wg.Go(func() {
			defer running.Store(false)
			if err := task(ctx); err != nil && h.OnError != nil {
				h.OnError(err)
			}
		})
	}

	scheduledAt := time.Now()
	start()
	for {
		scheduledAt = s.Next(scheduledAt)
		if scheduledAt.IsZero() {
			<-ctx.Done()
			return
		}
		if now := time.Now(); !scheduledAt.After(now) {
			// Slots were missed (wall-clock jump, suspend, a late wake-up):
			// report the first one and resume from now instead of firing
			// every missed slot in a burst.
			if h.OnOverrun != nil {
				h.OnOverrun(scheduledAt)
			}
			if scheduledAt = s.Next(now); scheduledAt.IsZero() {
				<-ctx.Done()
				return
			}
		}
		timer := time.NewTimer(time.Until(scheduledAt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		if running.Load() {
			if h.OnOverrun != nil {
				h.OnOverrun(scheduledAt)
			}
			continue
		}
		start()
	}
}

// Events names the log events and messages of a logged loop.
type Events struct {
	OverrunEvent, OverrunMsg string
	FailureEvent, FailureMsg string
}

// scheduledAtLayout is RFC 3339 with milliseconds; values are formatted in UTC.
const scheduledAtLayout = "2006-01-02T15:04:05.000Z07:00"

// RunLogged is Run with logging hooks: an overrun is logged as a warning with
// "event", attrs and "scheduledAt"; a task error is logged as an error with
// "event", attrs, "error" and, when present, "causes" (see Causes). A task
// error does not stop the loop.
func RunLogged(ctx context.Context, s cron.Schedule, task func(context.Context) error, log *slog.Logger, ev Events, attrs ...any) {
	with := func(event string, extra ...any) []any {
		args := make([]any, 0, 2+len(attrs)+len(extra))
		args = append(args, "event", event)
		args = append(args, attrs...)
		return append(args, extra...)
	}
	Run(ctx, s, task, Hooks{
		OnError: func(err error) {
			extra := []any{"error", err.Error()}
			if causes := Causes(err); causes != nil {
				extra = append(extra, "causes", causes)
			}
			log.Error(ev.FailureMsg, with(ev.FailureEvent, extra...)...)
		},
		OnOverrun: func(scheduledAt time.Time) {
			log.Warn(ev.OverrunMsg, with(ev.OverrunEvent, "scheduledAt", scheduledAt.UTC().Format(scheduledAtLayout))...)
		},
	})
}

// Causes returns the non-empty messages of the errors joined in err (an
// error implementing Unwrap() []error, e.g. errors.Join), or nil.
func Causes(err error) []string {
	var multi interface{ Unwrap() []error }
	if !errors.As(err, &multi) {
		return nil
	}
	var out []string
	for _, e := range multi.Unwrap() {
		if e == nil {
			continue
		}
		if msg := e.Error(); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}
