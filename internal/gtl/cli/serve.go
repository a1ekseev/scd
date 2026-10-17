package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/gtl/config"
	"github.com/a1ekseev/scd/internal/gtl/server"
)

// shutdownTimeout bounds how long active downloads may finish after a stop
// signal (the Node service waited without a deadline).
const shutdownTimeout = 10 * time.Minute

func serveCommand() *cobra.Command {
	return configCommand("serve", "Run the load generator HTTP server", func(cmd *cobra.Command, cfg config.Config) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serve(ctx, cfg, cmd.ErrOrStderr())
	})
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
	switch cause := context.Cause(ctx); {
	case cause == nil, cause == context.Canceled: //nolint:errorlint // sentinel identity
		return "context canceled"
	case strings.HasPrefix(cause.Error(), "interrupt "):
		return "SIGINT"
	case strings.HasPrefix(cause.Error(), "terminated "):
		return "SIGTERM"
	default:
		return cause.Error()
	}
}
