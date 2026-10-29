package wgpstatus

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/securefile"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	want := Status{Version: Version, UpdatedAt: at, Providers: []Provider{
		{ID: "a", Output: "out-a", Type: "static", State: StateOK, LastTickAt: at, LastSuccessAt: at, Issued: 2},
		{ID: "b", Output: "out-b", Type: "other", State: StateNeedsLogin, Error: "refresh rejected", LastTickAt: at, Issued: 3, Limit: 10, ExpiresAt: at.Add(time.Hour)},
	}}
	if err := Write(dir, Status{UpdatedAt: at, Providers: want.Providers}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, Name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	got, err := Read(dir, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRead(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		data    string
		max     int64
		wantErr string
		refused bool
	}{
		{name: "unknown fields", mode: 0o600, max: 1024, data: `{"version":1,"future":true,"providers":[{"id":"a","state":"ok","extra":{"x":1}}]}`},
		{name: "version 2", mode: 0o600, max: 1024, data: `{"version":2}`, wantErr: "unsupported version 2"},
		{name: "no version", mode: 0o600, max: 1024, data: `{}`, wantErr: "unsupported version 0"},
		{name: "bad json", mode: 0o600, max: 1024, data: `{`, wantErr: "parse status.json"},
		{name: "0644", mode: 0o644, max: 1024, data: `{"version":1}`, refused: true},
		{name: "over limit", mode: 0o600, max: 5, data: `{"version":1}`, wantErr: "status.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, Name)
			if err := os.WriteFile(p, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, tc.mode); err != nil {
				t.Fatal(err)
			}
			got, err := Read(dir, tc.max)
			switch {
			case tc.refused:
				if !errors.Is(err, securefile.ErrRefused) {
					t.Fatalf("err %v, want ErrRefused", err)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case len(got.Providers) != 1 || got.Providers[0].ID != "a":
				t.Fatalf("got %+v", got)
			}
		})
	}
}
