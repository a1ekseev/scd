package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/sfc/config"
	"github.com/a1ekseev/scd/internal/sfc/refresh"
)

func refreshCommand() *cobra.Command {
	return configCommand("refresh", "Refresh all outputs once and print the report", func(cmd *cobra.Command, l config.Loaded) error {
		return runRefresh(cmd.Context(), l, cmd.OutOrStdout(), cmd.ErrOrStderr())
	})
}

// RunRefresh loads the config at cfgPath, refreshes every enabled output once
// (reading and writing the source cache) and prints the report JSON to
// stdout; logs go to stderr. It returns cliutil.ErrReportFailed when an
// output failed.
func RunRefresh(ctx context.Context, cfgPath string, stdout, stderr io.Writer) error {
	l, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	return runRefresh(ctx, l, stdout, stderr)
}

func runRefresh(ctx context.Context, l config.Loaded, stdout, stderr io.Writer) error {
	log, err := logging.New(stderr, l.Config.Logging.Level, l.Config.Logging.Format)
	if err != nil {
		return err
	}
	r := &refresh.Refresher{Cfg: l, State: refresh.NewState(l.Config), Log: log}
	rep := r.Run(ctx)
	if err := jsonout.Write(stdout, rep); err != nil {
		return err
	}
	if rep.Failed > 0 {
		return cliutil.ErrReportFailed
	}
	return nil
}
