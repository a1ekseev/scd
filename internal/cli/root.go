// Package cli builds the root scd command and runs it.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cliutil"
	gtlcli "github.com/a1ekseev/scd/internal/gtl/cli"
	scdcli "github.com/a1ekseev/scd/internal/scd/cli"
	sfccli "github.com/a1ekseev/scd/internal/sfc/cli"
)

// NewRoot builds the root `scd` command with all service commands mounted.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "scd",
		Short:         "Generate and apply Xray resources from subscriptions.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(scdcli.Commands()...)
	root.AddCommand(sfccli.New(), gtlcli.New())
	return root
}

// Execute runs the root command and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	return run(NewRoot(), args, stdout, stderr)
}

func run(root *cobra.Command, args []string, stdout, stderr io.Writer) int {
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.Execute()
	if err == nil {
		return 0
	}
	if !errors.Is(err, cliutil.ErrReportFailed) {
		fmt.Fprintln(stderr, err.Error())
	}
	return 1
}
