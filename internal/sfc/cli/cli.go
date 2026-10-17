// Package cli provides the sfc service command tree.
package cli

import "github.com/spf13/cobra"

// New returns the `sfc` command.
func New() *cobra.Command {
	return &cobra.Command{
		Use:   "sfc",
		Short: "Filter upstream subscriptions and serve them by public URL.",
	}
}
