package cli

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cliutil"
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
//
// A failure after the logger exists is logged as service_failed and not
// printed again (cliutil.ErrLogged).
func serve(ctx context.Context, l config.Loaded, stderr io.Writer) error {
	log, err := logging.New(stderr, l.Config.Logging.Level, l.Config.Logging.Format)
	if err != nil {
		return err
	}
	return cliutil.LogFailure(log, serveWith(ctx, l, log))
}

// serveWith is serve with its logger.
func serveWith(ctx context.Context, l config.Loaded, log *slog.Logger) error {
	cfg := l.Config
	sched, err := cron.Parse(cfg.Runtime.RefreshSchedule)
	if err != nil {
		return err
	}
	log.Info("Config loaded.", "event", "config_loaded", "configPath", l.Path, "version", buildinfo.Version)

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

	// Log "stopping" as soon as ctx is done, before the server drains.
	logged := make(chan struct{})
	stopLog := context.AfterFunc(ctx, func() {
		defer close(logged)
		log.Info("Server stopping.", "event", "server_stopping", "signal", sigctx.Reason(ctx))
	})
	defer func() {
		if !stopLog() {
			<-logged
		}
	}()
	loopCtx, stopLoop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		var started atomic.Bool
		tick := func(ctx context.Context) error {
			if !started.Swap(true) {
				return nil // the start-up refresh already ran
			}
			log.Debug("Refresh tick started.", "event", "refresh_tick")
			if rep := r.Run(ctx); rep.Canceled != "" {
				// Shutdown, not a failure: debug only.
				log.Debug("Refresh tick canceled.", "event", "refresh_tick_canceled", "phase", rep.Canceled)
			}
			return nil
		}
		cronloop.RunLogged(loopCtx, sched, tick, log, refreshEvents)
	})
	err = httpserve.Run(ctx, ln, httpserve.Recover(log, server.New(r.State)), httpserve.ShutdownTimeout)
	stopLoop()
	wg.Wait()
	return err
}
