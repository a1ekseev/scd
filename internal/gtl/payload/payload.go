// Package payload generates the random load payload: an AES-128-CTR keystream
// under a per-process key and a per-request IV, so no two bodies repeat.
package payload

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
)

// ChunkSize is the length of each write to the response.
const ChunkSize = 65536

// Source produces payload bodies. It is safe for concurrent use.
type Source struct {
	block cipher.Block
}

// New returns a Source with a fresh random AES-128 key, held only in memory.
func New() *Source {
	key := make([]byte, 16)
	_, _ = rand.Read(key)          // never fails: crypto/rand crashes the process instead
	block, _ := aes.NewCipher(key) // 16-byte key: cannot fail
	return &Source{block: block}
}

// Write streams total random bytes to w in ChunkSize chunks using a single
// reused buffer, with a fresh random IV per call. It returns the first write
// error.
func (s *Source) Write(w io.Writer, total uint64) error {
	iv := make([]byte, aes.BlockSize)
	_, _ = rand.Read(iv)
	ctr := cipher.NewCTR(s.block, iv)
	buf := make([]byte, min(uint64(ChunkSize), max(total, 1)))
	for off := uint64(0); off < total; off += ChunkSize {
		b := buf[:min(uint64(ChunkSize), total-off)]
		clear(b) // the keystream XORs in place, so start from zeros every chunk
		ctr.XORKeyStream(b, b)
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}
