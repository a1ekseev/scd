package payload

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"testing"
)

func write(t *testing.T, s *Source, n uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := s.Write(&buf, n); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWriteLength(t *testing.T) {
	s := New()
	for _, n := range []uint64{0, 1, 16, 1024, ChunkSize, ChunkSize + 1, 65 * 1024, 98311} {
		if got := len(write(t, s, n)); uint64(got) != n {
			t.Errorf("size %d: wrote %d", n, got)
		}
	}
}

func TestWriteDiffers(t *testing.T) {
	s := New()
	a, b := write(t, s, 2*ChunkSize), write(t, s, 2*ChunkSize)
	if bytes.Equal(a, b) {
		t.Fatal("two calls on one source must differ")
	}
	if bytes.Equal(a[:ChunkSize], a[ChunkSize:]) {
		t.Fatal("chunks must differ")
	}
	if bytes.Equal(a, write(t, New(), 2*ChunkSize)) {
		t.Fatal("two sources must differ")
	}
}

func TestWriteIncompressible(t *testing.T) {
	b := write(t, New(), 1<<20)
	if bytes.Equal(b[:ChunkSize], make([]byte, ChunkSize)) {
		t.Fatal("payload is zeros")
	}
	var z bytes.Buffer
	fw, _ := flate.NewWriter(&z, flate.BestCompression)
	_, _ = fw.Write(b)
	_ = fw.Close()
	if z.Len() < len(b)*99/100 {
		t.Fatalf("compressed %d of %d bytes", z.Len(), len(b))
	}
}

type failingWriter struct{ calls int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls == 2 {
		return 0, errors.New("boom")
	}
	return len(p), nil
}

func TestWriteStopsOnWriterError(t *testing.T) {
	w := &failingWriter{}
	if err := New().Write(w, 10*ChunkSize); err == nil {
		t.Fatal("expected error")
	}
	if w.calls != 2 {
		t.Fatalf("expected stop at second write, got %d calls", w.calls)
	}
}

func BenchmarkWrite64MiB(b *testing.B) {
	const total = 64 << 20
	s := New()
	b.SetBytes(total)
	for b.Loop() {
		if err := s.Write(io.Discard, total); err != nil {
			b.Fatal(err)
		}
	}
}
