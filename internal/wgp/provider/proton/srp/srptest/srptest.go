// Package srptest is the server side of the Proton SRP-6a login, written
// from the protocol. Only tests import it: they use it as the API end of a
// login. internal/archtest keeps it out of non-test files.
package srptest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"math/big"
	"slices"

	"github.com/a1ekseev/scd/internal/wgp/provider/proton/pgp"
	"github.com/a1ekseev/scd/internal/wgp/provider/proton/srp"
)

const byteLength = 256

var (
	one = big.NewInt(1)
	two = big.NewInt(2)
)

// Server is the server of one login.
type Server struct {
	n, v, b, bigB *big.Int
}

// NewServer returns the server of one login to the account with password
// and salt, over the signed modulus.
func NewServer(signedModulus string, salt, password []byte) (*Server, error) {
	text, err := pgp.VerifyModulus(signedModulus)
	if err != nil {
		return nil, err
	}
	nBytes, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(nBytes) != byteLength {
		return nil, errors.New("modulus: not a 2048-bit base64 number")
	}
	n := fromLE(nBytes)
	k := fromLE(expandHash(append(toLE(two), nBytes...)))
	k.Mod(k, n)
	b, err := rand.Int(rand.Reader, new(big.Int).Sub(n, one))
	if err != nil {
		return nil, err
	}
	v := new(big.Int).Exp(two, fromLE(srp.HashPassword(password, salt, nBytes)), n)
	bigB := new(big.Int).Mul(k, v)
	bigB.Add(bigB, new(big.Int).Exp(two, b, n)).Mod(bigB, n)
	return &Server{n: n, v: v, b: b, bigB: bigB}, nil
}

// Ephemeral is B, 256 bytes little-endian.
func (s *Server) Ephemeral() []byte { return toLE(s.bigB) }

// Verify checks the client proof and returns the server proof.
func (s *Server) Verify(clientEphemeral, clientProof []byte) ([]byte, bool) {
	a := fromLE(clientEphemeral)
	if new(big.Int).Mod(a, s.n).Sign() == 0 {
		return nil, false
	}
	bBytes := toLE(s.bigB)
	u := fromLE(expandHash(slices.Concat(clientEphemeral, bBytes)))
	// S = (A·v^u)^b mod N
	base := new(big.Int).Exp(s.v, u, s.n)
	base.Mul(base, a).Mod(base, s.n)
	sBytes := toLE(base.Exp(base, s.b, s.n))
	if !bytes.Equal(clientProof, expandHash(slices.Concat(clientEphemeral, bBytes, sBytes))) {
		return nil, false
	}
	return expandHash(slices.Concat(clientEphemeral, clientProof, sBytes)), true
}

// expandHash is SHA-512(data‖0) ‖ … ‖ SHA-512(data‖3).
func expandHash(data []byte) []byte {
	var out []byte
	for i := range byte(4) {
		h := sha512.Sum512(slices.Concat(data, []byte{i}))
		out = append(out, h[:]...)
	}
	return out
}

func fromLE(b []byte) *big.Int {
	r := slices.Clone(b)
	slices.Reverse(r)
	return new(big.Int).SetBytes(r)
}

func toLE(v *big.Int) []byte {
	b := v.FillBytes(make([]byte, byteLength))
	slices.Reverse(b)
	return b
}
