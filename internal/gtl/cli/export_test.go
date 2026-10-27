package cli

import (
	"context"
	"io"
	"time"

	"github.com/a1ekseev/scd/internal/gtl/config"
)

// SetShutdownTimeout overrides the shutdown timeout and returns a restore func.
func SetShutdownTimeout(d time.Duration) func() {
	old := shutdownTimeout
	shutdownTimeout = d
	return func() { shutdownTimeout = old }
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
