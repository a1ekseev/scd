package payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type vector struct {
	SizeBytes *uint64 `json:"sizeBytes"`
	Sha256    string  `json:"sha256"`
	First16   string  `json:"first16hex"`
	Offset    *uint64 `json:"offset"`
	Fill8     string  `json:"fill8"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/reference/gtl/payload-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v []vector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFillVectors(t *testing.T) {
	n := 0
	for _, v := range loadVectors(t) {
		if v.Offset == nil {
			continue
		}
		n++
		b := make([]byte, 8)
		Fill(b, *v.Offset)
		if got := hex.EncodeToString(b); got != v.Fill8 {
			t.Errorf("offset %d: got %s want %s", *v.Offset, got, v.Fill8)
		}
	}
	if n != 4 {
		t.Fatalf("expected 4 fill vectors, got %d", n)
	}
}

func TestWriteSHA256Vectors(t *testing.T) {
	n := 0
	for _, v := range loadVectors(t) {
		if v.SizeBytes == nil {
			continue
		}
		n++
		var buf bytes.Buffer
		if err := Write(&buf, *v.SizeBytes); err != nil {
			t.Fatal(err)
		}
		if uint64(buf.Len()) != *v.SizeBytes {
			t.Errorf("size %d: wrote %d", *v.SizeBytes, buf.Len())
		}
		sum := sha256.Sum256(buf.Bytes())
		if got := hex.EncodeToString(sum[:]); got != v.Sha256 {
			t.Errorf("size %d: sha256 %s want %s", *v.SizeBytes, got, v.Sha256)
		}
		first := buf.Bytes()
		if len(first) > 16 {
			first = first[:16]
		}
		if got := hex.EncodeToString(first); got != v.First16 {
			t.Errorf("size %d: first16 %s want %s", *v.SizeBytes, got, v.First16)
		}
	}
	if n != 8 {
		t.Fatalf("expected 8 size vectors, got %d", n)
	}
}

func TestWriteDiffersByOffset(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, 2*ChunkSize); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if bytes.Equal(b[:ChunkSize], b[ChunkSize:]) {
		t.Fatal("chunks at different offsets must differ")
	}
}

func TestWriteNotASCIIa(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, 98311); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) != 98311 {
		t.Fatalf("len %d", len(b))
	}
	if bytes.Equal(b[:8], []byte("aaaaaaaa")) {
		t.Fatal("payload must not be ASCII a")
	}
	distinct := map[byte]bool{}
	for _, c := range b[:256] {
		distinct[c] = true
	}
	if len(distinct) <= 128 {
		t.Fatalf("only %d distinct bytes in first 256", len(distinct))
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
	if err := Write(w, 10*ChunkSize); err == nil {
		t.Fatal("expected error")
	}
	if w.calls != 2 {
		t.Fatalf("expected stop at second write, got %d calls", w.calls)
	}
}
