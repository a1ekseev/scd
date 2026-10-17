package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cliutil"
)

func TestHelpListsServiceCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errb.String())
	}
	for _, name := range []string{"sfc", "gtl"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("help does not mention %q:\n%s", name, out.String())
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute([]string{"nope"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), `unknown command "nope"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestVersionFlag(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "1.2.3"

	var out, errb bytes.Buffer
	if code := Execute([]string{"--version"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errb.String())
	}
	if out.String() != "1.2.3\n" {
		t.Fatalf("stdout = %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := Execute([]string{"gtl", "--version"}, &out, &errb); code != 1 {
		t.Fatalf("gtl --version exit code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "unknown flag: --version") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestReportFailedExitCode(t *testing.T) {
	root := NewRoot()
	root.AddCommand(&cobra.Command{
		Use: "stub",
		RunE: func(*cobra.Command, []string) error {
			return cliutil.ErrReportFailed
		},
	})

	var out, errb bytes.Buffer
	if code := run(root, []string{"stub"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr must be empty, got %q", errb.String())
	}
}
