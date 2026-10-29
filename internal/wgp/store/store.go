// Package store is the encrypted wgp state: one file per provider in the
// state directory, sealed with AES-256-GCM under the state key.
//
// A record file <dir>/<provider id>.state is magic | nonce | sealed JSON,
// with the additional data "wgp-state-v1|"+providerID, so a record copied
// under another provider id does not open. A record that does not open is an
// error and is never replaced by a fresh one. Errors name paths, never the
// key or record contents.
package store

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/a1ekseev/scd/internal/common/securefile"
)

const (
	magic = "WGPSTAT1"
	// recordSuffix names record files: <provider id>.state.
	recordSuffix = ".state"
	lockName     = "wgp.lock"
	// MaxRecordBytes bounds a record file; Save refuses larger data.
	MaxRecordBytes  = 1 << 20
	maxKeyFileBytes = 256
	aadPrefix       = "wgp-state-v1|"
)

// Store is an opened state directory.
type Store struct {
	dir     string
	aead    cipher.AEAD
	keySHA8 string
}

// Open checks the state directory and the key and opens every record in the
// directory. The directory must be a directory (not a symlink) owned by the
// process uid without group or other access; every file in it must be a
// regular file owned by the process uid without group or other access; the
// key file must hold the standard base64 of 32 bytes and have no group or
// other access. A record that does not open (wrong key, foreign or corrupted
// file) fails Open.
func Open(dir, keyFile string) (*Store, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	raw, err := securefile.ReadPrivate(keyFile, maxKeyFileBytes)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil || len(key) != 32 {
		// The decode error is dropped: it may quote key bytes.
		return nil, fmt.Errorf("state key %s: must be the base64 of 32 bytes", keyFile)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	s := &Store{dir: dir, aead: aead, keySHA8: hex.EncodeToString(sum[:4])}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if err := checkFile(p, info); err != nil {
			return nil, err
		}
		if id, ok := strings.CutSuffix(e.Name(), recordSuffix); ok && id != "" {
			if _, err := s.Record(id).Load(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// KeySHA8 is the first 8 hex characters of the SHA-256 of the key: the only
// form of the key that may be logged.
func (s *Store) KeySHA8() string { return s.keySHA8 }

// Record returns the record of providerID. It implements provider.Store.
func (s *Store) Record(providerID string) Record {
	return Record{s: s, id: providerID}
}

// Record is the state record of one provider.
type Record struct {
	s  *Store
	id string
}

func (r Record) path() string { return filepath.Join(r.s.dir, r.id+recordSuffix) }

func (r Record) aad() []byte { return []byte(aadPrefix + r.id) }

// Load returns the decrypted JSON record, nil when the provider has none.
func (r Record) Load() ([]byte, error) {
	p := r.path()
	b, err := securefile.ReadPrivate(p, MaxRecordBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state record: %w", err)
	}
	ns := r.s.aead.NonceSize()
	if len(b) < len(magic)+ns+r.s.aead.Overhead() {
		return nil, fmt.Errorf("state record %s is truncated; it is not recreated, restore it or remove it to start over", p)
	}
	if string(b[:len(magic)]) != magic {
		return nil, fmt.Errorf("state record %s is not a wgp state record; it is not recreated, restore it or remove it to start over", p)
	}
	nonce, sealed := b[len(magic):len(magic)+ns], b[len(magic)+ns:]
	data, err := r.s.aead.Open(nil, nonce, sealed, r.aad())
	if err != nil {
		return nil, fmt.Errorf("state record %s does not open with key sha8 %s (wrong key, a record of another provider or a corrupted file); it is not recreated, restore it or remove it to start over", p, r.s.keySHA8)
	}
	return data, nil
}

// Save replaces the record with the JSON data atomically (mode 0600).
func (r Record) Save(data []byte) error {
	if !json.Valid(data) {
		return fmt.Errorf("state record of provider %q: data is not JSON", r.id)
	}
	nonce := make([]byte, r.s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := make([]byte, 0, len(magic)+len(nonce)+len(data)+r.s.aead.Overhead())
	out = append(append(out, magic...), nonce...)
	out = r.s.aead.Seal(out, nonce, data, r.aad())
	if len(out) > MaxRecordBytes {
		return fmt.Errorf("state record of provider %q: %d bytes exceed %d", r.id, len(out), MaxRecordBytes)
	}
	return securefile.WriteAtomic(r.path(), out)
}

// Lock takes the exclusive lock of the state directory: `serve`, `login` and
// `logout` never run on one directory at once. The lock is flock(2) on
// <dir>/wgp.lock, which holds the pid of the holder; the kernel drops it when
// the holder exits in any way, so a lock left by a dead process never blocks
// and the file left behind is reused. A live holder makes Lock fail at once.
// The returned function releases the lock.
func Lock(dir string) (unlock func() error, err error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkFile(p, info); err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pid := make([]byte, 32)
			n, _ := f.ReadAt(pid, 0)
			return nil, fmt.Errorf("state dir %s is in use by another wgp process (pid %s in its own pid namespace); stop it first", dir, strings.TrimSpace(string(pid[:n])))
		}
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	if err := f.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return nil, err
	}
	return f.Close, nil
}

// checkDir requires a directory owned by the process uid without group or
// other access.
func checkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state dir %s is not a directory (%s)", dir, info.Mode().Type())
	}
	return checkOwnerMode("state dir", dir, info)
}

// checkFile requires a regular file owned by the process uid without group
// or other access.
func checkFile(p string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("state file %s is not a regular file (%s)", p, info.Mode().Type())
	}
	return checkOwnerMode("state file", p, info)
}

func checkOwnerMode(what, p string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s %s has mode %04o, group and others must have no access", what, p, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s %s: no owner information", what, p)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("%s %s is owned by uid %d, want the process uid %d", what, p, st.Uid, uid)
	}
	return nil
}
