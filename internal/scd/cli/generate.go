package cli

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

// generateCommand is `scd generate` (01 §14.1): it builds the manifest of
// one input without trimming or filters and under the default policy, and
// writes the manifest and the parse log. Nothing is written when the input
// has no valid entries; stdout gets the summary.
func generateCommand() *cobra.Command {
	var input, output, logPath string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Build an outbound manifest from a subscription",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loaded, err := inputsrc.Load(cmd.Context(), input, inputsrc.Options{Stdin: cmd.InOrStdin()})
			if err != nil {
				return err
			}
			m := manifest.Build(loaded.Content, loaded.Source, normalize.Policy{}, time.Now())
			if err := manifest.Validate(m, loaded.Source); err != nil {
				return err
			}
			if err := writeJSON(output, m); err != nil {
				return err
			}
			if err := writeJSON(logPath, m.ParseLog()); err != nil {
				return err
			}
			return jsonout.Write(cmd.OutOrStdout(), m.Summary)
		},
	}
	cmd.Flags().StringVar(&input, "input", "", `Subscription file, http(s) URL or "-" for stdin`)
	cmd.Flags().StringVar(&output, "output", "", "Manifest JSON file to write")
	cmd.Flags().StringVar(&logPath, "log", "", "Parse log JSON file to write")
	for _, f := range []string{"input", "output", "log"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

// writeJSON writes v to the file path (created or truncated).
func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := jsonout.Write(f, v); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
