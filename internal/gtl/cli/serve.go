package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/gtl/config"
	"github.com/a1ekseev/scd/internal/gtl/server"
)

// shutdownTimeout bounds how long active downloads may finish after a stop
// signal; a variable so tests can shorten it.
var shutdownTimeout = httpserve.ShutdownTimeout

func serveCommand() *cobra.Command {
	return configCommand("serve", "Run the load generator HTTP server", func(cmd *cobra.Command, path string, cfg config.Config) error {
		ctx, stop := sigctx.NotifyContext(cmd.Context())
		defer stop()
		return serve(ctx, path, cfg, cmd.ErrOrStderr())
	})
}

// serve runs the server until ctx is done. A failure after the logger
// exists is logged as service_failed and not printed again.
func serve(ctx context.Context, path string, cfg config.Config, stderr io.Writer) error {
	log, err := logging.New(stderr, cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return err
	}
	log.Info("Config loaded.", "event", "config_loaded", "configPath", path, "version", buildinfo.Version)
	return cliutil.LogFailure(log, serveWith(ctx, cfg, log))
}

// serveWith is serve with its logger.
func serveWith(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	ln, err := httpserve.Listen(cfg.Server.Listen) //nolint:contextcheck // binding a socket needs no cancellation
	if err != nil {
		return err
	}
	log.Info("Server started.", "event", "gtl_server_started", "listen", cfg.Server.Listen)

	// Log "stopping" as soon as ctx is done, before Shutdown drains requests.
	logged := make(chan struct{})
	stopLog := context.AfterFunc(ctx, func() {
		defer close(logged)
		log.Info("Server stopping.", "event", "gtl_server_stopping", "signal", sigctx.Reason(ctx))
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
