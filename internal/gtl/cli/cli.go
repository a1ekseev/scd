// Package cli provides the gtl service command tree.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/gtl/config"
)

// New returns the `gtl` command.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gtl",
		Short: "Gen Test Load",
	}
	cmd.AddCommand(
		configCommand("validate-config", "Validate config file", func(cmd *cobra.Command, _ config.Config) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Config is valid")
			return err
		}),
		configCommand("print-config", "Print normalized config", func(cmd *cobra.Command, cfg config.Config) error {
			return jsonout.Write(cmd.OutOrStdout(), cfg)
		}),
	)
	return cmd
}

// configCommand builds a command that takes the required --config flag, loads
// the gtl config and passes it to run.
func configCommand(use, short string, run func(*cobra.Command, config.Config) error) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			return run(cmd, cfg)
		},
	}
	cmd.Flags().StringVar(&path, "config", "", "Path to config.yml")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
