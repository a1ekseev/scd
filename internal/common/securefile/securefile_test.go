package securefile

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadPrivate(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok", 0o600, "secret")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		max     int64
		want    string
		refused bool
		wantErr bool
	}{
		{name: "0600", path: ok, max: 6, want: "secret"},
		{name: "0400", path: write("ro", 0o400, "x"), max: 1, want: "x"},
		{name: "empty", path: write("empty", 0o600, ""), max: 0, want: ""},
		{name: "0640", path: write("g", 0o640, "x"), max: 10, refused: true},
		{name: "0604", path: write("o", 0o604, "x"), max: 10, refused: true},
		{name: "symlink", path: link, max: 10, refused: true},
		{name: "directory", path: sub, max: 10, refused: true},
		{name: "fifo", path: fifo, max: 10, refused: true},
		{name: "negative limit", path: ok, max: -1, wantErr: true},
		{name: "max int64 limit", path: ok, max: math.MaxInt64, wantErr: true},
		{name: "over limit", path: ok, max: 5, wantErr: true},
		{name: "missing", path: filepath.Join(dir, "none"), max: 10, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadPrivate(tc.path, tc.max)
			if tc.refused || tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want error", got)
				}
				if errors.Is(err, ErrRefused) != tc.refused {
					t.Fatalf("errors.Is(%v, ErrRefused) = %v, want %v", err, !tc.refused, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteAtomic(t *testing.T) {
	tests := []struct {
		name     string
		existing *string
	}{
		{name: "new file"},
		{name: "overwrite", existing: new("old content that is longer")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "state.json")
			if tc.existing != nil {
				if err := os.WriteFile(p, []byte(*tc.existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := WriteAtomic(p, []byte("new")); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %04o, want 0600", info.Mode().Perm())
			}
			got, err := ReadPrivate(p, 100)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "new" {
				t.Fatalf("content = %q, want %q", got, "new")
			}
			assertOnly(t, dir, "state.json")
		})
	}
}

func TestWriteAtomicErrorLeavesNoTemp(t *testing.T) {
	t.Run("target is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "target"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := WriteAtomic(filepath.Join(dir, "target"), []byte("x")); err == nil {
			t.Fatal("want error")
		}
		assertOnly(t, dir, "target")
	})
	t.Run("temp file cannot be created", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := WriteAtomic(filepath.Join(dir, "f"), []byte("x")); err == nil {
			t.Fatal("want error")
		}
		assertOnly(t, dir)
	})
}

func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(names) {
		t.Fatalf("dir entries = %v, want %v", got, names)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Fatalf("dir entries = %v, want %v", got, names)
		}
	}
}
