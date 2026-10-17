package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// syncCommand is `scd sync` (03 §2.4): one sync with a fresh memory state.
// The report JSON goes to stdout and logs to stderr (D27 O10); failed
// items exit 1 without another message. SIGINT/SIGTERM cancel the run: the
// Xray apply in flight finishes, nothing new starts and the report of what
// was done is printed (D7).
func syncCommand() *cobra.Command {
	return configCommand("sync", "Sync subscriptions to Xray once and print the report", func(cmd *cobra.Command, l config.Loaded) error {
		ctx, stop := sigctx.NotifyContext(cmd.Context())
		defer stop()
		log, err := logging.New(cmd.ErrOrStderr(), l.Config.Logging.Level, l.Config.Logging.Format)
		if err != nil {
			return err
		}
		log.LogAttrs(ctx, slog.LevelInfo, "Config loaded.", slog.String("event", "config_loaded"), slog.String("configPath", l.Path))
		s := &syncer.Syncer{Config: l, Memory: state.New(), Log: log, Dial: xrayapi.Dial}
		report, err := s.Run(ctx)
		if err != nil {
			log.LogAttrs(ctx, slog.LevelError, "Sync failed.", slog.String("event", "sync_failed"), slog.String("error", err.Error()))
			return err
		}
		if err := jsonout.Write(cmd.OutOrStdout(), report); err != nil {
			return err
		}
		if report.Failed > 0 {
			return cliutil.ErrReportFailed
		}
		return nil
	})
}
