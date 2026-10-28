// Package securefile reads private files with strict permission checks and
// writes files atomically with mode 0600.
package securefile

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// ErrRefused is wrapped by ReadPrivate errors for a file that is not a
// regular file or is accessible by group or others.
var ErrRefused = errors.New("refused")

// ReadPrivate reads the regular file at path. It refuses symlinks,
// directories and other non-regular files, and files with any of the
// permission bits 0o077 set. Content longer than maxBytes is an error.
func ReadPrivate(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || maxBytes == math.MaxInt64 {
		return nil, fmt.Errorf("read %s: invalid limit %d", path, maxBytes)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(path, info); err != nil {
		return nil, err
	}
	// O_NONBLOCK: a FIFO swapped in after Lstat must not block Open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The path may have been swapped between Lstat and Open.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("file %s changed while opening: %w", path, ErrRefused)
	}
	if err := checkPrivate(path, opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %s exceeds %d bytes", path, maxBytes)
	}
	return data, nil
}

func checkPrivate(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file %s is not a regular file (%s): %w", path, info.Mode().Type(), ErrRefused)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("file %s has mode %04o, group and others must have no access: %w", path, perm, ErrRefused)
	}
	return nil
}

// WriteAtomic replaces the file at path with data, mode 0600. It writes a
// temporary file in the same directory, syncs it, renames it over path and
// syncs the directory. On any error the temporary file is removed; if only
// the directory sync fails, path already holds the new data. Temporary files
// left by a crashed process are not cleaned up.
func WriteAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
