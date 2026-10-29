package srptest

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"os"
	"testing"

	"github.com/a1ekseev/scd/internal/wgp/provider/proton/srp"
)

// The server accepts the client proofs of srp on random passwords of 1–96
// bytes, and the client accepts only its server proof.
func TestMiniVerifier(t *testing.T) {
	var signed []string
	for _, f := range []string{"modulus-1.asc", "modulus-2.asc"} {
		b, err := os.ReadFile("../testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		signed = append(signed, string(b))
	}
	b64 := base64.StdEncoding.EncodeToString
	for i := range 24 {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			lenB, _ := rand.Int(rand.Reader, big.NewInt(96))
			password := make([]byte, 1+lenB.Int64())
			salt := make([]byte, 10)
			rand.Read(password)
			rand.Read(salt)
			m := signed[i%2]
			srv, err := NewServer(m, salt, password)
			if err != nil {
				t.Fatal(err)
			}
			c := srp.Challenge{Version: 3 + i%2, Modulus: m, ServerEphemeral: b64(srv.Ephemeral()), Salt: b64(salt)}
			p, err := srp.NewProofs(c, password)
			if err != nil {
				t.Fatal(err)
			}
			m2, ok := srv.Verify(p.ClientEphemeral, p.ClientProof)
			if !ok {
				t.Fatalf("server rejected the proof for a %d-byte password", len(password))
			}
			if err := p.VerifyServerProof(m2); err != nil {
				t.Fatal(err)
			}
			m2[0] ^= 1
			if err := p.VerifyServerProof(m2); err == nil {
				t.Fatal("wrong server proof accepted")
			}
			if err := p.VerifyServerProof(nil); err == nil {
				t.Fatal("empty server proof accepted")
			}
			wrong := append(bytes.Clone(password), 'x')
			if pw, err := srp.NewProofs(c, wrong); err != nil {
				t.Fatal(err)
			} else if _, ok := srv.Verify(pw.ClientEphemeral, pw.ClientProof); ok && len(password) < 72 {
				t.Fatal("server accepted a wrong password")
			}
		})
	}
}
