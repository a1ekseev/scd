package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/scd/config"
)

func validateConfigCommand() *cobra.Command {
	return configCommand("validate-config", "Validate config file", func(cmd *cobra.Command, l config.Loaded) error {
		return jsonout.Write(cmd.OutOrStdout(), struct {
			ConfigPath string `json:"configPath"`
			Valid      bool   `json:"valid"`
		}{l.Path, true})
	})
}

func printConfigCommand() *cobra.Command {
	return configCommand("print-config", "Print normalized config", func(cmd *cobra.Command, l config.Loaded) error {
		return jsonout.Write(cmd.OutOrStdout(), l.Config)
	})
}

// configCommand builds a command that takes the required --config flag,
// loads the scd config, prints its warnings to stderr as "warning: …" and
// passes it to run.
func configCommand(use, short string, run func(*cobra.Command, config.Loaded) error) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, warnings, err := config.Load(path)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w); err != nil {
					return err
				}
			}
			return run(cmd, l)
		},
	}
	cmd.Flags().StringVar(&path, "config", "", "Path to config.yml")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}
