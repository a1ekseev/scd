// Package cli provides the gtl service command tree.
package cli

import "github.com/spf13/cobra"

// New returns the `gtl` command.
func New() *cobra.Command {
	return &cobra.Command{
		Use:   "gtl",
		Short: "Gen Test Load",
	}
}
