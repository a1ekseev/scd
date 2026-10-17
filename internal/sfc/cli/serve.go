package cli

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/cronloop"
	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/sfc/config"
	"github.com/a1ekseev/scd/internal/sfc/refresh"
	"github.com/a1ekseev/scd/internal/sfc/server"
)

// refreshEvents are the cron loop log events (04 §7.1).
var refreshEvents = cronloop.Events{
	OverrunEvent: "refresh_tick_skipped_overrun",
	OverrunMsg:   "Refresh tick skipped because previous run is still in progress.",
	FailureEvent: "refresh_failed",
	FailureMsg:   "Refresh tick failed.",
}

func serveCommand() *cobra.Command {
	return configCommand("serve", "Refresh outputs on schedule and serve them over HTTP", func(cmd *cobra.Command, l config.Loaded) error {
		ctx, stop := sigctx.NotifyContext(cmd.Context())
		defer stop()
		return serve(ctx, l, cmd.ErrOrStderr())
	})
}

// serve runs the initial refresh before the HTTP server listens, so outputs
// are ready once it accepts requests (a bind error is reported only after
// it). The cron loop then refreshes from the first scheduled slot; its
// immediate start-up tick is skipped so the start-up refresh runs once
// (O12). When ctx is done the loop stops at once and waits only for an
// active refresh, while the server drains for up to
// httpserve.ShutdownTimeout (D7).
func serve(ctx context.Context, l config.Loaded, stderr io.Writer) error {
	cfg := l.Config
	log, err := logging.New(stderr, cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return err
	}
	sched, err := cron.Parse(cfg.Runtime.RefreshSchedule)
	if err != nil {
		return err
	}
	log.Info("Config loaded.", "event", "config_loaded", "configPath", l.Path)

	r := &refresh.Refresher{Cfg: l, State: refresh.NewState(cfg), Log: log}
	r.Run(ctx)
	if ctx.Err() != nil {
		return nil
	}

	ln, err := httpserve.Listen(cfg.Server.Listen) //nolint:contextcheck // binding a socket needs no cancellation
	if err != nil {
		return err
	}
	log.Info("Subscription server started.", "event", "server_started", "listen", cfg.Server.Listen)

	loopCtx, stopLoop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		var started atomic.Bool
		tick := func(ctx context.Context) error {
			if !started.Swap(true) {
				return nil // the start-up refresh already ran
			}
			log.Info("Refresh tick started.", "event", "refresh_tick")
			r.Run(ctx)
			return nil
		}
		cronloop.RunLogged(loopCtx, sched, tick, log, refreshEvents)
	})
	err = httpserve.Run(ctx, ln, httpserve.Recover(log, server.New(r.State)), httpserve.ShutdownTimeout)
	stopLoop()
	wg.Wait()
	return err
}
