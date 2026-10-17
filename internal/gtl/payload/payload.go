// Package payload generates the deterministic xorshift32 load payload.
package payload

import "io"

// ChunkSize is the payload chunk length; the generator is reseeded at the
// start of every chunk.
const ChunkSize = 65536

// Fill fills b with payload bytes for a chunk starting at the absolute body
// offset. Only the low 32 bits of offset seed the generator.
func Fill(b []byte, offset uint64) {
	s := uint32(0x9e3779b9) ^ uint32(offset)
	for i := range b {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		b[i] = byte(s)
	}
}

// Write streams total payload bytes to w in ChunkSize chunks using a single
// reused buffer. It returns the first write error.
func Write(w io.Writer, total uint64) error {
	buf := make([]byte, min(uint64(ChunkSize), max(total, 1)))
	for off := uint64(0); off < total; off += ChunkSize {
		n := min(uint64(ChunkSize), total-off)
		Fill(buf[:n], off)
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
	}
	return nil
}
