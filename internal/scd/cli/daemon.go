package cli

import (
	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/daemon"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// daemonCommand is `scd daemon` (03 §3): sync, monitoring, balancer
// selection, remote collection and the status server on their schedules
// until SIGINT or SIGTERM, then an immediate graceful stop (D7). A second
// signal during the stop terminates the process at once. A failure after
// the logger exists is logged as service_failed and not printed again.
func daemonCommand() *cobra.Command {
	return configCommand("daemon", "Run sync, monitoring and the status server on schedule", func(cmd *cobra.Command, l config.Loaded) error {
		ctx, stop := sigctx.NotifyContext(cmd.Context())
		defer stop()
		log, err := logging.New(cmd.ErrOrStderr(), l.Config.Logging.Level, l.Config.Logging.Format)
		if err != nil {
			return err
		}
		d := &daemon.Daemon{Config: l, Log: log, Dial: xrayapi.Dial}
		return cliutil.LogFailure(log, d.Run(ctx))
	})
}
