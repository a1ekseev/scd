package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEscapeID(t *testing.T) {
	tests := []struct{ id, want string }{
		{"main", "main"},
		{"../main/slash id", "..%2Fmain%2Fslash%20id"},
		{"x+y", "x%2By"},
		{"a&b=c:d@e$f", "a%26b%3Dc%3Ad%40e%24f"},
		{"Германия", "%D0%93%D0%B5%D1%80%D0%BC%D0%B0%D0%BD%D0%B8%D1%8F"},
		{"a-b_c.d~e", "a-b_c.d~e"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := EscapeID(tt.id); got != tt.want {
				t.Errorf("EscapeID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// TestPathEscapesID is 04 §11 test 22.
func TestPathEscapesID(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	const id = "../main/slash id"

	p := Path(configPath, id)
	if want := filepath.Join(dir, ".sfc-cache", "..%2Fmain%2Fslash%20id.subscription"); p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
	if got := Dir(configPath); filepath.Dir(p) != got {
		t.Fatalf("Path %q is not inside Dir %q", p, got)
	}

	written, err := Write(configPath, id, "vless://x")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written != p {
		t.Fatalf("Write returned %q, want %q", written, p)
	}
	gotPath, content, err := Read(configPath, id)
	if err != nil || gotPath != p || content != "vless://x" {
		t.Fatalf("Read = %q, %q, %v", gotPath, content, err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	for _, content := range []string{"first\nline", "second", ""} {
		if _, err := Write(configPath, "main", content); err != nil {
			t.Fatalf("Write(%q): %v", content, err)
		}
		_, got, err := Read(configPath, "main")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got != content {
			t.Fatalf("Read = %q, want %q", got, content)
		}
	}
	entries, err := os.ReadDir(Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "main.subscription" {
		t.Fatalf("cache dir entries = %v, want only main.subscription", entries)
	}
}

func TestReadMissing(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	p, _, err := Read(configPath, "main")
	if err == nil {
		t.Fatal("Read of a missing cache succeeded")
	}
	if p != Path(configPath, "main") {
		t.Fatalf("Read path = %q", p)
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("error = %v, want no such file", err)
	}
}

// TestWriteRenameFailureRemovesTemp is 04 §11 test 23.
func TestWriteRenameFailureRemovesTemp(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yml")
	p := Path(configPath, "main")
	if err := os.MkdirAll(filepath.Join(p, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(configPath, "main", "content"); err == nil {
		t.Fatal("Write over a directory succeeded")
	}
	tmps, err := filepath.Glob(filepath.Join(Dir(configPath), "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmps) != 0 {
		t.Fatalf("temp files left: %v", tmps)
	}
}
