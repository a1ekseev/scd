// Package crosstest checks the paths between services that share only files:
// what wgp writes is read by scd byte for byte.
package crosstest

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/daemon"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	_ "github.com/a1ekseev/scd/internal/wgp/provider/providertest"
	"github.com/a1ekseev/scd/internal/wgp/store"
)

// fixture returns the lines of a candidate fixture that scd accepts (the
// fixture also holds broken and duplicate lines) and them as a YAML list.
func fixture(t *testing.T, kind candidate.Kind) (lines []string, yaml string) {
	t.Helper()
	b, err := os.ReadFile("../../docker/fixtures/" + string(kind) + "/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Split(string(b), "\n")
	var quoted []string
	for _, e := range manifest.BuildCandidates(kind, string(b), "fixture", "f", "UseIPv4", time.Unix(0, 0)).Entries {
		lines = append(lines, all[e.Line-1])
		quoted = append(quoted, strconv.Quote(all[e.Line-1]))
	}
	if len(lines) < 2 {
		t.Fatalf("fixture %s: %d lines", kind, len(lines))
	}
	return lines, "[" + strings.Join(quoted, ", ") + "]"
}

func parse(kind candidate.Kind, line string) (any, error) {
	if kind == candidate.KindMasque {
		return candidate.ParseMasque(line)
	}
	return candidate.ParseWireGuard(line)
}

// TestWGPToSCD runs a fake provider per kind through the wgp daemon and
// reads its output and status.json the way scd reads them.
func TestWGPToSCD(t *testing.T) {
	root := t.TempDir()
	stateDir, outDir := filepath.Join(root, "state"), filepath.Join(root, "out")
	for _, d := range []string{stateDir, outDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := filepath.Join(root, "key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	wg, wgYAML := fixture(t, candidate.KindWireGuard)
	mq, mqYAML := fixture(t, candidate.KindMasque)
	cfgPath := filepath.Join(root, "config.yml")
	cfg := `server: {listen: 127.0.0.1:8080}
state: {dir: ` + stateDir + `, keyFile: ` + keyFile + `}
identification: {fake: {timezone: UTC}}
deviceNamePrefix: wgp-a-
providers:
  - {id: wg, type: fake, schedule: "@hourly", kind: wireguard, lines: ` + wgYAML + `}
  - {id: mq, type: fake, schedule: "@hourly", kind: masque, lines: ` + mqYAML + `}
outputs:
  - {id: wg, path: ` + outDir + `/wg, provider: wg, candidates: 1}
  - {id: mq, path: ` + outDir + `/mq, provider: mq, candidates: 1}
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(l.Config, func(id string) provider.Store { return st.Record(id) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wgr sync.WaitGroup
	wgr.Go(func() { d.Run(ctx) })
	for deadline := time.Now().Add(5 * time.Second); len(d.Status().Providers) < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", d.Status())
		}
	}
	cancel()
	wgr.Wait()

	// status.json: the reader scd uses.
	status, err := wgpstatus.Read(outDir, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Providers) != 2 {
		t.Fatalf("status %+v", status)
	}
	for _, p := range status.Providers {
		if p.State != wgpstatus.StateOK || p.Type != "fake" || p.Output != p.ID {
			t.Errorf("status %+v", p)
		}
	}

	// The output files: scd loads them as private files and builds the
	// manifest of the source; every published line is an entry.
	for _, tc := range []struct {
		kind  candidate.Kind
		path  string
		lines []string
	}{
		{candidate.KindWireGuard, outDir + "/wg", wg},
		{candidate.KindMasque, outDir + "/mq", mq},
	} {
		loaded, err := manifest.LoadPrivate(tc.path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Content != strings.Join(tc.lines, "\n")+"\n" {
			t.Fatalf("%s: bytes differ from the provider lines", tc.path)
		}
		for i, line := range strings.Split(strings.TrimSuffix(loaded.Content, "\n"), "\n") {
			if _, err := parse(tc.kind, line); err != nil {
				t.Errorf("%s line %d: %v", tc.path, i+1, err)
			}
		}
		m := manifest.BuildCandidates(tc.kind, loaded.Content, tc.path, string(tc.kind), "UseIPv4", time.Unix(0, 0))
		if len(m.Skipped) != 0 || m.Summary.Parsed != len(tc.lines) {
			t.Errorf("%s: parsed %d of %d, skipped %+v", tc.path, m.Summary.Parsed, len(tc.lines), m.Skipped)
		}
	}
}
