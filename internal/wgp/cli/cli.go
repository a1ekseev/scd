// Package cli provides the wgp service command tree.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/daemon"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	_ "github.com/a1ekseev/scd/internal/wgp/provider/all" // links every provider type
	"github.com/a1ekseev/scd/internal/wgp/server"
	"github.com/a1ekseev/scd/internal/wgp/store"
)

// New returns the `wgp` command.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wgp",
		Short: "Manage provider accounts and publish WireGuard and MASQUE candidates.",
	}
	cmd.AddCommand(
		configCommand("serve", "Run providers on schedule and publish their outputs", func(cmd *cobra.Command, l config.Loaded) error {
			ctx, stop := sigctx.NotifyContext(cmd.Context())
			defer stop()
			return serve(ctx, l, cmd.ErrOrStderr())
		}),
		interactiveCommand("login", "Sign in to a provider interactively (wgp serve stopped)"),
		interactiveCommand("logout", "Revoke what a provider issued and close its session"),
		configCommand("validate-config", "Validate config file", func(cmd *cobra.Command, l config.Loaded) error {
			return jsonout.Write(cmd.OutOrStdout(), struct {
				ConfigPath string `json:"configPath"`
				Valid      bool   `json:"valid"`
			}{l.Path, true})
		}),
		configCommand("print-config", "Print normalized config (paths to secrets, never their contents)", func(cmd *cobra.Command, l config.Loaded) error {
			return jsonout.Write(cmd.OutOrStdout(), l.Config)
		}),
	)
	return cmd
}

// serve locks and opens the state, listens on server.listen, then runs the
// provider loops and the status page until ctx is done. On a stop signal the
// loops stop at once and wait for an active tick; the server drains for up
// to httpserve.ShutdownTimeout (D7). The state lock is released last.
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
	enabled := 0
	for _, p := range l.Config.Providers {
		if p.Enabled {
			enabled++
		}
	}
	log.Info("Config loaded.", "event", "config_loaded", "configPath", l.Path, "providers", len(l.Config.Providers),
		"enabledProviders", enabled, "version", buildinfo.Version)
	st, unlock, err := openState(l.Config)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	log.Info("State opened.", "event", "state_opened", "stateDir", l.Config.State.Dir, "keySha8", st.KeySHA8())
	d, err := daemon.New(l.Config, func(id string) provider.Store { return st.Record(id) }, log)
	if err != nil {
		return err
	}
	ln, err := httpserve.Listen(l.Config.Server.Listen) //nolint:contextcheck // binding a socket needs no cancellation
	if err != nil {
		return err
	}
	log.Info("Status server started.", "event", "server_started", "listen", l.Config.Server.Listen)

	// Log "stopping" as soon as ctx is done, before the loops and the server
	// drain.
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
	loopCtx, stopLoops := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { d.Run(loopCtx) })
	err = httpserve.Run(ctx, ln, httpserve.Recover(log, server.New(d.Status)), httpserve.ShutdownTimeout)
	stopLoops()
	wg.Wait()
	return err
}

// openState takes the state dir lock, then opens the store.
func openState(cfg config.Config) (*store.Store, func() error, error) {
	unlock, err := store.Lock(cfg.State.Dir)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.State.Dir, cfg.State.KeyFile)
	if err != nil {
		_ = unlock()
		return nil, nil, err
	}
	return st, unlock, nil
}

func interactiveCommand(use, short string) *cobra.Command {
	var id, hvToken string
	cmd := configCommand(use, short, func(cmd *cobra.Command, l config.Loaded) error {
		i, err := interactive(l.Config, id)
		if err != nil {
			return err
		}
		st, unlock, err := openState(l.Config)
		if err != nil {
			return err
		}
		defer func() { _ = unlock() }()
		ctx, stop := sigctx.NotifyContext(cmd.Context())
		defer stop()
		if use == "login" {
			return i.Login(ctx, provider.TTY{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), HVToken: hvToken}, st.Record(id))
		}
		return i.Logout(ctx, st.Record(id))
	})
	cmd.Flags().StringVar(&id, "provider", "", "Provider id")
	_ = cmd.MarkFlagRequired("provider")
	if use == "login" {
		cmd.Flags().StringVar(&hvToken, "hv-token", "", "Human verification token from the link of an earlier login attempt")
	}
	return cmd
}

// interactive returns the provider id of cfg as Interactive, or an error
// when there is no such provider or it has no login.
func interactive(cfg config.Config, id string) (provider.Interactive, error) {
	p, ok := cfg.Provider(id)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", id)
	}
	i, ok := p.Impl.(provider.Interactive)
	if !ok {
		return nil, fmt.Errorf("provider %q (type %q) does not support login", p.ID, p.Type)
	}
	return i, nil
}

// configCommand builds a command that takes the required --config flag, loads
// the wgp config and passes it to run.
func configCommand(use, short string, run func(*cobra.Command, config.Loaded) error) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := config.Load(path)
			if err != nil {
				return err
			}
			return run(cmd, l)
		},
	}
	cmd.Flags().StringVar(&path, "config", "", "Path to config.yml")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
