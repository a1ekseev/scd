// Package cli provides the scd service commands mounted on the root command.
package cli

import "github.com/spf13/cobra"

// Commands returns the scd top-level commands.
func Commands() []*cobra.Command {
	return []*cobra.Command{generateCommand(), validateConfigCommand(), printConfigCommand()}
}
