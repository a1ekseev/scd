// Package cache stores the last decoded content of each sfc subscription on
// disk (<config dir>/.sfc-cache), so outputs can be rebuilt when the upstream
// is unavailable, including after a restart (04 §6).
package cache

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// dirName is the cache directory next to the config file. The name is kept
// from the TS version: the docker volume survives the migration.
const dirName = ".sfc-cache"

// EscapeID escapes an id like JavaScript encodeURIComponent (Go additionally
// escapes !'()*). It names cache files and builds output URL paths; "/" and
// ".." can therefore never leave the cache directory.
func EscapeID(id string) string {
	return strings.ReplaceAll(url.QueryEscape(id), "+", "%20")
}

// Dir returns the cache directory for the config at configPath.
func Dir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), dirName)
}

// Path returns the cache file of the subscription id.
func Path(configPath, subscriptionID string) string {
	return filepath.Join(Dir(configPath), EscapeID(subscriptionID)+".subscription")
}

// Write atomically replaces the cache file of the subscription id with
// content (a temp file renamed into place) and returns the file path. On
// failure the temp file is removed.
func Write(configPath, id, content string) (string, error) {
	p := Path(configPath, id)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return p, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(p)+".*.tmp")
	if err != nil {
		return p, err
	}
	tmp := f.Name()
	_, werr := f.WriteString(content)
	err = errors.Join(werr, f.Close())
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return p, err
	}
	return p, nil
}

// Read returns the cache file path of the subscription id and its content.
// An empty file is not an error.
func Read(configPath, id string) (path, content string, err error) {
	path = Path(configPath, id)
	b, err := os.ReadFile(path)
	return path, string(b), err
}
