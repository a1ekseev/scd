package sigctx_test

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/sigctx"
)

func TestNotifyContextSignals(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
	}{{syscall.SIGINT, "SIGINT"}, {syscall.SIGTERM, "SIGTERM"}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := sigctx.NotifyContext(t.Context())
			defer stop()
			if err := syscall.Kill(syscall.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("context not cancelled by signal")
			}
			if got := sigctx.Reason(ctx); got != tc.name {
				t.Fatalf("Reason = %q, want %q", got, tc.name)
			}
			if got := context.Cause(ctx).Error(); got != tc.name+" received" {
				t.Fatalf("cause = %q", got)
			}
		})
	}
}

func TestReasonPlainCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := sigctx.Reason(ctx); got != "context canceled" {
		t.Fatalf("Reason = %q", got)
	}
}

func TestStopCancelsWithoutSignalCause(t *testing.T) {
	ctx, stop := sigctx.NotifyContext(t.Context())
	stop()
	<-ctx.Done()
	if got := sigctx.Reason(ctx); got != "context canceled" {
		t.Fatalf("Reason = %q", got)
	}
}

func TestParentCancelPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := sigctx.NotifyContext(parent)
	defer stop()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancel did not propagate")
	}
	if got := sigctx.Reason(ctx); got != "context canceled" {
		t.Fatalf("Reason = %q", got)
	}
}
