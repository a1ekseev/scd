package cli

import (
	"context"
	"io"

	"github.com/a1ekseev/scd/internal/sfc/config"
)

// RunServe loads the config at cfgPath, refreshes the outputs once, then
// serves them until ctx is done, logging to stderr.
func RunServe(ctx context.Context, cfgPath string, stderr io.Writer) error {
	l, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	return serve(ctx, l, stderr)
}

// RunRefresh loads the config at cfgPath, refreshes every enabled output once
// and prints the report JSON to stdout; logs go to stderr.
func RunRefresh(ctx context.Context, cfgPath string, stdout, stderr io.Writer) error {
	l, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	return runRefresh(ctx, l, stdout, stderr)
}
