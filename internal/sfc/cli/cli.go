// Package cli provides the sfc service command tree.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/sfc/config"
)

// New returns the `sfc` command.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sfc",
		Short: "Filter upstream subscriptions and serve them by public URL.",
	}
	cmd.AddCommand(
		serveCommand(),
		refreshCommand(),
		configCommand("validate-config", "Validate config file", func(cmd *cobra.Command, l config.Loaded) error {
			return jsonout.Write(cmd.OutOrStdout(), struct {
				ConfigPath string `json:"configPath"`
				Valid      bool   `json:"valid"`
			}{l.Path, true})
		}),
		configCommand("print-config", "Print normalized config", func(cmd *cobra.Command, l config.Loaded) error {
			return jsonout.Write(cmd.OutOrStdout(), l.Config)
		}),
	)
	return cmd
}

// configCommand builds a command that takes the required --config flag, loads
// the sfc config and passes it to run.
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
