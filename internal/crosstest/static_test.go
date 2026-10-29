package crosstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/output"
	_ "github.com/a1ekseev/scd/internal/wgp/provider/all"
)

// TestStaticToSCD publishes the tracked static fixtures through the static
// provider and the wgp output, and builds the scd manifest of the same bytes.
func TestStaticToSCD(t *testing.T) {
	root := t.TempDir()
	src, staticDir, outDir := "../../docker/fixtures/static", filepath.Join(root, "static"), filepath.Join(root, "out")
	for _, d := range []string{staticDir, outDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// git keeps the fixtures 0644; static refuses files open to others.
		if err := os.WriteFile(filepath.Join(staticDir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(root, "config.yml")
	cfg := `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
deviceNamePrefix: wgp-a-
providers:
  - {id: static, type: static, schedule: "@hourly", dir: ` + staticDir + `}
outputs:
  - {id: wg, path: ` + outDir + `/wg, provider: static}
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Config.Providers[0].Impl
	res, err := p.Tick(t.Context(), nil)
	if err != nil || len(res.Lines) != len(entries) || len(res.Warnings) == 0 {
		t.Fatalf("tick: err %v, %d lines of %d files, warnings %q", err, len(res.Lines), len(entries), res.Warnings)
	}
	path := outDir + "/wg"
	if _, err := output.Publish(output.Output{ID: "wg", Path: path, Kind: p.Kind()}, res.Lines); err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.LoadPrivate(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Content != strings.Join(res.Lines, "\n")+"\n" {
		t.Fatal("bytes differ from the provider lines")
	}
	m := manifest.BuildCandidates(candidate.KindWireGuard, loaded.Content, path, "wg", "UseIPv4", time.Unix(0, 0))
	if len(m.Skipped) != 0 || m.Summary.Parsed != len(entries) {
		t.Fatalf("parsed %d of %d, skipped %+v", m.Summary.Parsed, len(entries), m.Skipped)
	}
	for i, line := range res.Lines {
		w, err := candidate.ParseWireGuard(line)
		if err != nil || w.Provider != "static" || w.Label == "" {
			t.Fatalf("line %d: %v %v", i+1, w, err)
		}
	}
}
