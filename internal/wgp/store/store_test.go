package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/wgp/provider"
)

var _ provider.Store = Record{}

// fixture makes a state dir and a key file, and returns their paths.
func fixture(t *testing.T, keyByte byte) (dir, keyFile string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(root, "key")
	writeKey(t, keyFile, keyByte)
	return dir, keyFile
}

func writeKey(t *testing.T, path string, b byte) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) + "\n"
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T, dir, keyFile string) *Store {
	t.Helper()
	s, err := Open(dir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	dir, keyFile := fixture(t, 1)
	s := open(t, dir, keyFile)
	r := s.Record("a")
	if got, err := r.Load(); err != nil || got != nil {
		t.Fatalf("empty Load = %q, %v", got, err)
	}
	secret := []byte(`{"token":"fake-secret-token"}`)
	if err := r.Save(secret); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "a.state"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(magic)) || bytes.Contains(raw, []byte("fake-secret-token")) {
		t.Fatalf("record is not sealed: %q", raw)
	}
	info, err := os.Stat(filepath.Join(dir, "a.state"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v, %v", info.Mode(), err)
	}
	// A fresh Open checks and reads the record back.
	got, err := open(t, dir, keyFile).Record("a").Load()
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("Load = %q, %v", got, err)
	}
	if err := r.Save([]byte("not json")); err == nil {
		t.Fatal("Save of non-JSON data succeeded")
	}
	if err := r.Save([]byte(`"` + strings.Repeat("x", MaxRecordBytes) + `"`)); err == nil {
		t.Fatal("Save over the size limit succeeded")
	}
	if s.KeySHA8() != "72cd6e84" {
		t.Fatalf("KeySHA8 = %q", s.KeySHA8())
	}
}

// Every way a record fails to open fails both Load and Open, and the error
// carries neither the key nor the contents.
func TestUnreadableRecords(t *testing.T) {
	tests := []struct {
		name string
		// corrupt changes the saved record of provider "a".
		corrupt func(t *testing.T, dir, keyFile string)
		want    string
	}{
		{"foreign aad", func(t *testing.T, dir, _ string) {
			if err := os.Rename(filepath.Join(dir, "a.state"), filepath.Join(dir, "b.state")); err != nil {
				t.Fatal(err)
			}
		}, "b.state does not open"},
		{"wrong key", func(t *testing.T, _, keyFile string) { writeKey(t, keyFile, 2) }, "a.state does not open with key sha8"},
		{"truncated", func(t *testing.T, dir, _ string) {
			if err := os.Truncate(filepath.Join(dir, "a.state"), 20); err != nil {
				t.Fatal(err)
			}
		}, "a.state is truncated"},
		{"truncated tag", func(t *testing.T, dir, _ string) {
			p := filepath.Join(dir, "a.state")
			info, _ := os.Stat(p)
			if err := os.Truncate(p, info.Size()-1); err != nil {
				t.Fatal(err)
			}
		}, "a.state does not open"},
		{"bad magic", func(t *testing.T, dir, _ string) {
			p := filepath.Join(dir, "a.state")
			b, _ := os.ReadFile(p)
			b[0] ^= 1
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "a.state is not a wgp state record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, keyFile := fixture(t, 1)
			if err := open(t, dir, keyFile).Record("a").Save([]byte(`{"token":"fake-secret-token"}`)); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, dir, keyFile)
			_, err := Open(dir, keyFile)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "it is not recreated") {
				t.Fatalf("Open error %v, want %q", err, tc.want)
			}
			key, _ := os.ReadFile(keyFile)
			if strings.Contains(err.Error(), "fake-secret-token") || strings.Contains(err.Error(), strings.TrimSpace(string(key))) {
				t.Fatalf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestPermissions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, keyFile string) (string, string)
		want  string
	}{
		{"dir 0750", func(t *testing.T, dir, keyFile string) (string, string) {
			chmod(t, dir, 0o750)
			return dir, keyFile
		}, "has mode 0750"},
		{"dir symlink", func(t *testing.T, dir, keyFile string) (string, string) {
			link := dir + "-link"
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			return link, keyFile
		}, "is not a directory"},
		{"missing dir", func(_ *testing.T, dir, keyFile string) (string, string) {
			return dir + "-none", keyFile
		}, "no such file"},
		{"file 0640", func(t *testing.T, dir, keyFile string) (string, string) {
			p := filepath.Join(dir, "other")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			chmod(t, p, 0o640)
			return dir, keyFile
		}, "other has mode 0640"},
		{"subdir", func(t *testing.T, dir, keyFile string) (string, string) {
			if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir, keyFile
		}, "sub is not a regular file"},
		{"key 0640", func(t *testing.T, dir, keyFile string) (string, string) {
			chmod(t, keyFile, 0o640)
			return dir, keyFile
		}, "has mode 0640"},
		{"short key", func(t *testing.T, dir, keyFile string) (string, string) {
			if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(make([]byte, 16))), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir, keyFile
		}, "must be the base64 of 32 bytes"},
		{"key not base64", func(t *testing.T, dir, keyFile string) (string, string) {
			if err := os.WriteFile(keyFile, []byte("not-base64!"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir, keyFile
		}, "must be the base64 of 32 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, keyFile := fixture(t, 1)
			dir, keyFile = tc.setup(t, dir, keyFile)
			if _, err := Open(dir, keyFile); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open error %v, want %q", err, tc.want)
			}
		})
	}
	// Owner checks need another uid, which only root can set; they run in
	// the same checkOwnerMode as the mode checks.
}

func chmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLock(t *testing.T) {
	dir, _ := fixture(t, 1)
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	if _, err := Lock(dir); err == nil || !strings.Contains(err.Error(), "in use by another wgp process (pid "+pid) {
		t.Fatalf("second Lock error %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	// The lock file stays and stays private, so Open accepts it.
	info, err := os.Stat(filepath.Join(dir, lockName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file %v, %v", info, err)
	}

	bad, _ := fixture(t, 1)
	chmod(t, bad, 0o755)
	if _, err := Lock(bad); err == nil || !strings.Contains(err.Error(), "has mode 0755") {
		t.Fatalf("Lock of an open dir: %v", err)
	}
}

// A lock file left by a dead process holds its pid but no flock: the next
// Lock takes it over and writes its own pid.
func TestStaleLock(t *testing.T) {
	dir, _ := fixture(t, 1)
	p := filepath.Join(dir, lockName)
	if err := os.WriteFile(p, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock over a stale lock file: %v", err)
	}
	defer func() { _ = unlock() }()
	b, err := os.ReadFile(p)
	if err != nil || string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock file %q, %v", b, err)
	}
}
