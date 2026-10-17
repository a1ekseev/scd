package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/gtl/config"
	"github.com/a1ekseev/scd/internal/gtl/server"
)

// shutdownTimeout bounds how long active downloads may finish after a stop
// signal; a variable so tests can shorten it.
var shutdownTimeout = httpserve.ShutdownTimeout

func serveCommand() *cobra.Command {
	return configCommand("serve", "Run the load generator HTTP server", func(cmd *cobra.Command, cfg config.Config) error {
		ctx, stop := notifyContext(cmd.Context())
		defer stop()
		return serve(ctx, cfg, cmd.ErrOrStderr())
	})
}

// signalCause is the context cause recorded when a stop signal arrives.
type signalCause string

func (c signalCause) Error() string { return string(c) + " received" }

// notifyContext returns a context cancelled by the first SIGINT or SIGTERM,
// with a signalCause naming it. Right after the first signal the default
// signal handling is restored, so a second signal during the shutdown drain
// terminates the process immediately.
func notifyContext(parent context.Context) (context.Context, context.CancelFunc) {
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

// RunServe loads the config at cfgPath and serves until ctx is done, logging
// to stderr. It returns nil after a clean stop.
func RunServe(ctx context.Context, cfgPath string, stderr io.Writer) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, stderr)
}

func serve(ctx context.Context, cfg config.Config, stderr io.Writer) error {
	log, err := logging.New(stderr, cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return err
	}
	ln, err := httpserve.Listen(cfg.Server.Listen) //nolint:contextcheck // binding a socket needs no cancellation
	if err != nil {
		return err
	}
	log.Info("Server started.", "event", "gtl_server_started", "listen", cfg.Server.Listen)

	// Log "stopping" as soon as ctx is done, before Shutdown drains requests.
	logged := make(chan struct{})
	stopLog := context.AfterFunc(ctx, func() {
		defer close(logged)
		log.Info("Server stopping.", "event", "gtl_server_stopping", "signal", stopReason(ctx))
	})
	if err := httpserve.Run(ctx, ln, httpserve.Recover(log, server.New(cfg)), shutdownTimeout); err != nil {
		stopLog()
		return err
	}
	if !stopLog() {
		<-logged
	}
	log.Info("Server stopped.", "event", "gtl_server_stopped")
	return nil
}

// stopReason names the signal that cancelled ctx, or "context canceled".
func stopReason(ctx context.Context) string {
	if sc, ok := errors.AsType[signalCause](context.Cause(ctx)); ok {
		return string(sc)
	}
	return "context canceled"
}
