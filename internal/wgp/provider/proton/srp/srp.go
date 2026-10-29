// Package srp is the client side of the Proton API login: SRP-6a with auth
// versions 3 and 4. Numbers are 256-byte little-endian, g = 2, H is four
// SHA-512 digests of the data with a counter byte, and the password hash is
// bcrypt (cost 10, salt of 10 bytes + "proton") followed by H with N. The
// modulus N is used only after its OpenPGP signature by the Proton key is
// verified. Errors never carry the password, x or the session key.
package srp

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"slices"

	"golang.org/x/crypto/blowfish" //nolint:staticcheck // deprecated as a cipher; bcrypt with a caller-given salt needs the raw eksblowfish steps

	"github.com/a1ekseev/scd/internal/wgp/provider/proton/pgp"
)

const (
	bitLength  = 2048
	byteLength = bitLength / 8
	saltLength = 10
	// bcryptPrefix and bcryptCost: bcrypt "2y" with cost 10.
	bcryptPrefix = "$2y$10$"
	bcryptCost   = 10
	// maxKey is how much of the password bcrypt uses.
	maxKey = 72
)

var (
	one = big.NewInt(1)
	two = big.NewInt(2)
	// bcrypt64 is the bcrypt base64 alphabet, without padding.
	bcrypt64 = base64.NewEncoding("./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789").WithPadding(base64.NoPadding)
)

// Challenge is the server data of POST /auth/info.
type Challenge struct {
	// Version is the auth version; only 3 and 4 are supported.
	Version int
	// Modulus is the base64 N, OpenPGP cleartext-signed.
	Modulus string
	// ServerEphemeral is the base64 B.
	ServerEphemeral string
	// Salt is the base64 10-byte salt.
	Salt string
}

// Proofs is the client answer for POST /auth and the server proof it expects.
type Proofs struct {
	// ClientEphemeral is A, 256 bytes little-endian.
	ClientEphemeral []byte
	// ClientProof is M1.
	ClientProof []byte
	serverProof []byte
}

// NewProofs computes the client proofs for password with a fresh random
// client secret.
func NewProofs(c Challenge, password []byte) (*Proofs, error) {
	return newProofs(c, password, nil)
}

// VerifyServerProof checks the ServerProof from POST /auth in constant time.
func (p *Proofs) VerifyServerProof(serverProof []byte) error {
	if len(p.serverProof) == 0 {
		return errors.New("no expected server proof")
	}
	if subtle.ConstantTimeCompare(p.serverProof, serverProof) != 1 {
		return errors.New("server proof mismatch")
	}
	return nil
}

// String hides the proofs from logs.
func (p *Proofs) String() string { return "srp.Proofs{redacted}" }

// Format hides the proofs from every fmt verb.
func (p *Proofs) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, p.String()) }

// LogValue hides the proofs from log/slog.
func (p *Proofs) LogValue() slog.Value { return slog.StringValue(p.String()) }

// newProofs computes the proofs with the client secret a, or a random one
// when a is nil.
//
// shortcut: math/big is not constant-time, so the timing of one login may
// leak bits of x and a to a local observer; acceptable for an interactive
// login once in a while, switch to a constant-time bignum if logins become
// frequent or automated.
func newProofs(c Challenge, password []byte, a *big.Int) (*Proofs, error) {
	if c.Version != 3 && c.Version != 4 {
		return nil, fmt.Errorf("unsupported auth version %d", c.Version)
	}
	text, err := pgp.VerifyModulus(c.Modulus)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	nBytes, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(nBytes) != byteLength {
		return nil, errors.New("modulus: not a 2048-bit base64 number")
	}
	n := fromLE(nBytes)
	if err := checkModulus(n); err != nil {
		return nil, err
	}
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil || len(salt) != saltLength {
		return nil, fmt.Errorf("salt: not %d base64 bytes", saltLength)
	}
	bBytes, err := base64.StdEncoding.DecodeString(c.ServerEphemeral)
	if err != nil || len(bBytes) != byteLength {
		return nil, errors.New("server ephemeral: not a 2048-bit base64 number")
	}
	nMinus1 := new(big.Int).Sub(n, one)
	b := fromLE(bBytes)
	if b.Cmp(one) <= 0 || b.Cmp(nMinus1) >= 0 { // also B mod N != 0
		return nil, errors.New("server ephemeral out of range")
	}
	k := fromLE(expandHash(append(toLE(two), nBytes...)))
	k.Mod(k, n)
	if k.Cmp(one) <= 0 || k.Cmp(nMinus1) >= 0 {
		return nil, errors.New("multiplier out of range")
	}

	xBytes := HashPassword(password, salt, nBytes)
	x := fromLE(xBytes)
	clear(xBytes)
	defer func() { clear(x.Bits()) }()
	if a == nil {
		lower := big.NewInt(2 * bitLength) // keeps g^a above N
		for a == nil || a.Cmp(lower) <= 0 {
			if a, err = rand.Int(rand.Reader, nMinus1); err != nil {
				return nil, err
			}
		}
	}
	defer func() { clear(a.Bits()) }()
	aBytes := toLE(new(big.Int).Exp(two, a, n))
	u := fromLE(expandHash(concat(aBytes, bBytes)))
	if u.Sign() == 0 {
		return nil, errors.New("scrambling parameter is zero")
	}
	// S = (B - k·g^x)^(u·x + a) mod N
	base := new(big.Int).Exp(two, x, n)
	base.Mul(base, k).Sub(b, base).Mod(base, n)
	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, a).Mod(exp, nMinus1)
	sBytes := toLE(base.Exp(base, exp, n))
	clear(base.Bits())
	clear(exp.Bits())
	in1 := concat(aBytes, bBytes, sBytes)
	m1 := expandHash(in1)
	in2 := concat(aBytes, m1, sBytes)
	m2 := expandHash(in2)
	clear(sBytes)
	clear(in1)
	clear(in2)
	return &Proofs{ClientEphemeral: aBytes, ClientProof: m1, serverProof: m2}, nil
}

// checkModulus checks that N is a 2048-bit safe prime with N ≡ 3 mod 8, so
// that 2 generates the whole group.
func checkModulus(n *big.Int) error {
	if n.BitLen() != bitLength {
		return errors.New("modulus: wrong size")
	}
	if new(big.Int).Mod(n, big.NewInt(8)).Int64() != 3 {
		return errors.New("modulus: not 3 mod 8")
	}
	half := new(big.Int).Rsh(n, 1)
	if !half.ProbablyPrime(10) {
		return errors.New("modulus: not a safe prime")
	}
	// with (N-1)/2 prime, 2^((N-1)/2) ≡ -1 proves N prime (Lucas test)
	if new(big.Int).Exp(two, half, n).Cmp(new(big.Int).Sub(n, one)) != 0 {
		return errors.New("modulus: not prime")
	}
	return nil
}

// HashPassword is x = H(bcrypt("$2y$10$", salt ‖ "proton", password) ‖ N),
// the SRP password hash for the 256-byte little-endian modulus nBytes.
func HashPassword(password, salt, nBytes []byte) []byte {
	salt16 := slices.Concat(salt, []byte("proton"))
	key := slices.Concat(password[:min(len(password), maxKey)], []byte{0})
	key = key[:min(len(key), maxKey)]
	defer clear(key)
	c, err := blowfish.NewSaltedCipher(key, salt16)
	if err != nil {
		panic(err) // the key is never empty and the salt never short
	}
	for range 1 << bcryptCost {
		blowfish.ExpandKey(key, c)
		blowfish.ExpandKey(salt16, c)
	}
	ct := []byte("OrpheanBeholderScryDoubt")
	for i := 0; i < len(ct); i += blowfish.BlockSize {
		for range 64 {
			c.Encrypt(ct[i:i+blowfish.BlockSize], ct[i:i+blowfish.BlockSize])
		}
	}
	crypted := concat([]byte(bcryptPrefix+bcrypt64.EncodeToString(salt16)+bcrypt64.EncodeToString(ct[:23])), nBytes)
	defer clear(crypted)
	clear(ct)
	return expandHash(crypted)
}

// expandHash is SHA-512(data‖0) ‖ … ‖ SHA-512(data‖3), 256 bytes.
func expandHash(data []byte) []byte {
	out := make([]byte, 0, 4*sha512.Size)
	buf := slices.Concat(data, []byte{0})
	defer clear(buf)
	for i := range byte(4) {
		buf[len(buf)-1] = i
		h := sha512.Sum512(buf)
		out = append(out, h[:]...)
	}
	return out
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func fromLE(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i, v := range b {
		r[len(b)-1-i] = v
	}
	return new(big.Int).SetBytes(r)
}

// toLE is v as 256 bytes little-endian; v < 2^2048.
func toLE(v *big.Int) []byte {
	b := v.FillBytes(make([]byte, byteLength))
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}
