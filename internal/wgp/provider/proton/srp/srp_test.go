package srp

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"testing"
)

var b64 = base64.StdEncoding.EncodeToString

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVectors(t *testing.T) {
	var vectors []struct {
		Name, Modulus, PasswordHex, Salt, ClientSecretHex string
		ServerEphemeral, HashedPassword, ClientEphemeral  string
		ClientProof, ServerProof                          string
		Version                                           int
	}
	if err := json.Unmarshal(readFile(t, "vectors.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 10 {
		t.Fatalf("%d vectors", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			password, _ := hex.DecodeString(v.PasswordHex)
			aBytes, _ := hex.DecodeString(v.ClientSecretHex)
			signed := string(readFile(t, v.Modulus))
			c := Challenge{Version: v.Version, Modulus: signed, ServerEphemeral: v.ServerEphemeral, Salt: v.Salt}
			p, err := newProofs(c, password, new(big.Int).SetBytes(aBytes))
			if err != nil {
				t.Fatal(err)
			}
			salt, _ := base64.StdEncoding.DecodeString(v.Salt)
			if got := b64(HashPassword(password, salt, modulusBytes(t, signed))); got != v.HashedPassword {
				t.Errorf("x mismatch")
			}
			if b64(p.ClientEphemeral) != v.ClientEphemeral {
				t.Errorf("client ephemeral mismatch")
			}
			if b64(p.ClientProof) != v.ClientProof {
				t.Errorf("client proof mismatch")
			}
			sp, _ := base64.StdEncoding.DecodeString(v.ServerProof)
			if err := p.VerifyServerProof(sp); err != nil {
				t.Error(err)
			}
		})
	}
}

func modulusBytes(t *testing.T, signed string) []byte {
	t.Helper()
	lines := strings.Split(signed, "\n")
	n, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRejects(t *testing.T) {
	signed := string(readFile(t, "modulus-1.asc"))
	nBytes := modulusBytes(t, signed)
	n := fromLE(nBytes)
	salt := b64(make([]byte, saltLength))
	goodB := b64(toLE(big.NewInt(12345)))
	b := func(v *big.Int) string { return b64(toLE(v)) }
	cases := map[string]Challenge{
		"version 0":           {Version: 0, Modulus: signed, ServerEphemeral: goodB, Salt: salt},
		"version 1":           {Version: 1, Modulus: signed, ServerEphemeral: goodB, Salt: salt},
		"version 2":           {Version: 2, Modulus: signed, ServerEphemeral: goodB, Salt: salt},
		"version 5":           {Version: 5, Modulus: signed, ServerEphemeral: goodB, Salt: salt},
		"B = 0":               {Version: 4, Modulus: signed, ServerEphemeral: b(big.NewInt(0)), Salt: salt},
		"B = 1":               {Version: 4, Modulus: signed, ServerEphemeral: b(one), Salt: salt},
		"B = N":               {Version: 4, Modulus: signed, ServerEphemeral: b(n), Salt: salt},
		"B = N-1":             {Version: 4, Modulus: signed, ServerEphemeral: b(new(big.Int).Sub(n, one)), Salt: salt},
		"short B":             {Version: 4, Modulus: signed, ServerEphemeral: b64([]byte{5}), Salt: salt},
		"short salt":          {Version: 4, Modulus: signed, ServerEphemeral: goodB, Salt: b64([]byte{1, 2})},
		"garbage after sig":   {Version: 4, Modulus: signed + "garbage", ServerEphemeral: goodB, Salt: salt},
		"substituted modulus": {Version: 4, Modulus: strings.Replace(signed, "W2z5", "W2z6", 1), ServerEphemeral: goodB, Salt: salt},
		"unsigned modulus":    {Version: 4, Modulus: b64(nBytes), ServerEphemeral: goodB, Salt: salt},
	}
	if _, err := NewProofs(Challenge{Version: 4, Modulus: signed, ServerEphemeral: goodB, Salt: salt}, []byte("pw")); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for name, c := range cases {
		_, err := NewProofs(c, []byte("secret-password"))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-password") {
			t.Errorf("%s: error has the password: %v", name, err)
		}
		if strings.HasPrefix(name, "version") && !strings.Contains(err.Error(), "unsupported auth version") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckModulus(t *testing.T) {
	n := fromLE(modulusBytes(t, string(readFile(t, "modulus-2.asc"))))
	if err := checkModulus(n); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]*big.Int{
		"not prime":   new(big.Int).Add(n, big.NewInt(8)),
		"not 3 mod 8": new(big.Int).Add(n, big.NewInt(2)),
		"short":       new(big.Int).Rsh(n, 8),
	} {
		if err := checkModulus(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPasswordTruncation(t *testing.T) {
	salt := make([]byte, saltLength)
	n := make([]byte, byteLength)
	long := bytes.Repeat([]byte("p"), maxKey)
	if !bytes.Equal(HashPassword(long, salt, n), HashPassword(append(long, "tail"...), salt, n)) {
		t.Fatal("bytes after 72 changed the hash")
	}
	if bytes.Equal(HashPassword(long[:71], salt, n), HashPassword(append(long[:71], 'q'), salt, n)) {
		t.Fatal("byte 72 did not change the hash")
	}
}

func TestZeroProofs(t *testing.T) {
	var p Proofs
	if err := p.VerifyServerProof(nil); err == nil {
		t.Fatal("zero Proofs accepted an empty server proof")
	}
	if err := p.VerifyServerProof(make([]byte, byteLength)); err == nil {
		t.Fatal("zero Proofs accepted a server proof")
	}
}

func TestProofsRedacted(t *testing.T) {
	p := &Proofs{ClientEphemeral: []byte("AAAA"), ClientProof: []byte("BBBB"), serverProof: []byte("CCCC")}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "p", p)
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%+v %#v %s %x", p, p, p, p), p.String(), buf.String()} {
		if strings.Contains(s, "AAAA") || strings.Contains(s, "BBBB") || strings.Contains(s, "CCCC") ||
			strings.Contains(s, "41414141") || !strings.Contains(s, "redacted") {
			t.Errorf("not redacted: %s", s)
		}
	}
}
