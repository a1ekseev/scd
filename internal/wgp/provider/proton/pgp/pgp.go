// Package pgp is the minimal OpenPGP needed for the Proton SRP modulus: it
// verifies a cleartext-signed message (armor, one v4 signature packet, tag 2)
// made with a v4 EdDSA Ed25519 public key (tag 6) over SHA-256 or SHA-512.
// Anything else, including data after the signed block, is an error.
package pgp

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/sha1"
	_ "crypto/sha256" // registers crypto.SHA256
	_ "crypto/sha512" // registers crypto.SHA512
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// protonModulusKey is the public key packet (tag 6, v4, EdDSA Ed25519) of the
// key that signs the Proton SRP moduli, fingerprint
// 248097092b458509c508dac0350585c4e9518f26.
var protonModulusKey = mustHex("c633045c01cb8316092b06010401da470f0101074016ead65d7c234de9aa8c3ec25e35d5c8a7f4a1fee7f42b662fcbfd7a79338201")

const (
	beginMessage   = "-----BEGIN PGP SIGNED MESSAGE-----"
	beginSignature = "-----BEGIN PGP SIGNATURE-----"
	endSignature   = "-----END PGP SIGNATURE-----"

	tagSignature = 2
	tagPublicKey = 6
	algoEdDSA    = 22
	sigText      = 0x01
)

// oidEd25519 is the curve OID of a legacy EdDSA Ed25519 key.
var oidEd25519 = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01}

var errSignature = errors.New("invalid signature")

// VerifyModulus checks that signed is a cleartext message signed by the
// Proton modulus key and returns its signed text.
func VerifyModulus(signed string) (string, error) {
	return verify(signed, protonModulusKey)
}

// verify checks a cleartext-signed message against one public key packet.
func verify(signed string, keyPacket []byte) (string, error) {
	pub, _, err := parseKey(keyPacket)
	if err != nil {
		return "", fmt.Errorf("key: %w", err)
	}
	text, hashes, sig, err := parseCleartext(signed)
	if err != nil {
		return "", err
	}
	body, err := onePacket(sig, tagSignature)
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	if err := checkSignature(body, []byte(text), hashes, pub, time.Now()); err != nil {
		return "", err
	}
	return strings.ReplaceAll(text, "\r\n", "\n"), nil
}

// parseKey returns the Ed25519 key and the v4 fingerprint of a tag 6 packet.
func parseKey(packet []byte) (ed25519.PublicKey, [20]byte, error) {
	var fp [20]byte
	b, err := onePacket(packet, tagPublicKey)
	if err != nil {
		return nil, fp, err
	}
	// version, creation time, algorithm, OID, MPI 0x40 ‖ point (263 bits)
	if len(b) != 1+4+1+1+len(oidEd25519)+2+1+ed25519.PublicKeySize || b[0] != 4 || b[5] != algoEdDSA ||
		int(b[6]) != len(oidEd25519) || !bytes.Equal(b[7:7+len(oidEd25519)], oidEd25519) {
		return nil, fp, errors.New("not a v4 EdDSA Ed25519 public key")
	}
	m := b[7+len(oidEd25519):]
	if binary.BigEndian.Uint16(m) != 263 || m[2] != 0x40 {
		return nil, fp, errors.New("not a v4 EdDSA Ed25519 public key")
	}
	h := sha1.New() // the v4 fingerprint is SHA-1 by definition
	h.Write([]byte{0x99, byte(len(b) >> 8), byte(len(b))})
	h.Write(b)
	copy(fp[:], h.Sum(nil))
	return ed25519.PublicKey(m[3:]), fp, nil
}

// parseCleartext splits a cleartext-signed message into the signed text
// (canonical: CRLF line ends, no trailing blanks, dash-escaping removed) and
// the binary signature, and the hashes the Hash headers declare (only
// SHA256 and SHA512 are allowed; no header would mean MD5, so one is
// required).
func parseCleartext(s string) (string, []crypto.Hash, []byte, error) {
	lines := strings.Split(strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	bad := func(what string) (string, []crypto.Hash, []byte, error) {
		return "", nil, nil, errors.New("cleartext message: " + what)
	}
	var hashes []crypto.Hash
	i := 0
	next := func() (string, bool) {
		if i >= len(lines) {
			return "", false
		}
		i++
		return lines[i-1], true
	}
	if l, _ := next(); l != beginMessage {
		return bad("no begin line")
	}
	for {
		l, ok := next()
		if !ok {
			return bad("truncated")
		}
		if l == "" {
			break
		}
		names, ok := strings.CutPrefix(l, "Hash: ")
		if !ok {
			return bad("unexpected header")
		}
		for name := range strings.SplitSeq(names, ",") {
			switch strings.TrimSpace(name) {
			case "SHA256":
				hashes = append(hashes, crypto.SHA256)
			case "SHA512":
				hashes = append(hashes, crypto.SHA512)
			default:
				return bad("unsupported Hash header")
			}
		}
	}
	if len(hashes) == 0 {
		return bad("no Hash header")
	}
	var text []string
	for {
		l, ok := next()
		if !ok {
			return bad("no signature")
		}
		if l == beginSignature {
			break
		}
		if strings.HasPrefix(l, "-") {
			var escaped bool
			if l, escaped = strings.CutPrefix(l, "- "); !escaped {
				return bad("line not dash-escaped")
			}
		}
		l = strings.TrimRight(l, " \t")
		text = append(text, l)
	}
	for { // armor headers
		l, ok := next()
		if !ok {
			return bad("truncated")
		}
		if l == "" {
			break
		}
		if !strings.Contains(l, ": ") {
			return bad("bad armor header")
		}
	}
	var b64 strings.Builder
	var crc string
	for {
		l, ok := next()
		if !ok {
			return bad("no end line")
		}
		if l == endSignature {
			break
		}
		if crc != "" {
			return bad("data after checksum")
		}
		if c, isCRC := strings.CutPrefix(l, "="); isCRC {
			crc = c
			continue
		}
		b64.WriteString(l)
	}
	if i != len(lines) {
		return bad("data after the signature")
	}
	sig, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return bad("bad armor")
	}
	if crc != "" {
		want, err := base64.StdEncoding.DecodeString(crc)
		if err != nil || len(want) != 3 || crc24(sig) != uint32(want[0])<<16|uint32(want[1])<<8|uint32(want[2]) {
			return bad("bad armor checksum")
		}
	}
	return strings.Join(text, "\r\n"), hashes, sig, nil
}

// onePacket returns the body of the only packet in b, which must have tag.
func onePacket(b []byte, tag byte) ([]byte, error) {
	if len(b) < 2 || b[0]&0x80 == 0 {
		return nil, errors.New("bad packet header")
	}
	var t byte
	var n, off int
	if b[0]&0x40 != 0 { // new format
		t = b[0] & 0x3f
		switch o := int(b[1]); {
		case o < 192:
			n, off = o, 2
		case o < 224 && len(b) >= 3:
			n, off = (o-192)<<8+int(b[2])+192, 3
		case o == 255 && len(b) >= 6:
			n, off = int(binary.BigEndian.Uint32(b[2:6])), 6
		default:
			return nil, errors.New("unsupported packet length")
		}
	} else { // old format
		t = (b[0] >> 2) & 0x0f
		switch lt := b[0] & 3; {
		case lt == 0:
			n, off = int(b[1]), 2
		case lt == 1 && len(b) >= 3:
			n, off = int(binary.BigEndian.Uint16(b[1:3])), 3
		case lt == 2 && len(b) >= 5:
			n, off = int(binary.BigEndian.Uint32(b[1:5])), 5
		default:
			return nil, errors.New("unsupported packet length")
		}
	}
	if t != tag {
		return nil, fmt.Errorf("packet tag %d, want %d", t, tag)
	}
	if n != len(b)-off {
		return nil, errors.New("packet length does not match the data")
	}
	return b[off:], nil
}

// checkSignature verifies a v4 EdDSA text signature packet body over text
// with one of the declared hashes, valid at now.
func checkSignature(b, text []byte, hashes []crypto.Hash, pub ed25519.PublicKey, now time.Time) error {
	if len(b) < 6 || b[0] != 4 || b[1] != sigText || b[2] != algoEdDSA {
		return errors.New("signature: not a v4 EdDSA text signature")
	}
	var hash crypto.Hash
	switch b[3] {
	case 8:
		hash = crypto.SHA256
	case 10:
		hash = crypto.SHA512
	default:
		return fmt.Errorf("signature: unsupported hash algorithm %d", b[3])
	}
	if !slices.Contains(hashes, hash) {
		return errors.New("signature: hash does not match the Hash header")
	}
	hashedEnd := 6 + int(binary.BigEndian.Uint16(b[4:6]))
	if len(b) < hashedEnd+2 {
		return errors.New("signature: truncated")
	}
	if err := checkSubpackets(b[6:hashedEnd], now); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	unhashedEnd := hashedEnd + 2 + int(binary.BigEndian.Uint16(b[hashedEnd:]))
	if len(b) < unhashedEnd+2 {
		return errors.New("signature: truncated")
	}
	rest := b[unhashedEnd:]
	left16, rest := rest[:2], rest[2:]
	r, rest, okR := mpi(rest)
	s, rest, okS := mpi(rest)
	if !okR || !okS || len(rest) != 0 {
		return errors.New("signature: bad EdDSA value")
	}
	h := hash.New()
	h.Write(text)
	h.Write(b[:hashedEnd])
	h.Write([]byte{4, 0xff})
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(hashedEnd)))
	digest := h.Sum(nil)
	if !bytes.Equal(digest[:2], left16) || !ed25519.Verify(pub, digest, append(r, s...)) {
		return errSignature
	}
	return nil
}

// Signature subpacket types this package understands.
const (
	subCreationTime   = 2
	subExpirationTime = 3
	subIssuer         = 16
	subIssuerFP       = 33
)

// checkSubpackets rejects an unknown critical subpacket and an expired
// signature.
func checkSubpackets(b []byte, now time.Time) error {
	var created, expires uint32
	for len(b) > 0 {
		var n, off int
		switch o := int(b[0]); {
		case o < 192:
			n, off = o, 1
		case o < 255 && len(b) >= 2:
			n, off = (o-192)<<8+int(b[1])+192, 2
		case o == 255 && len(b) >= 5:
			n, off = int(binary.BigEndian.Uint32(b[1:5])), 5
		default:
			return errors.New("bad subpacket length")
		}
		if n < 1 || len(b) < off+n {
			return errors.New("bad subpacket length")
		}
		typ, data := b[off]&0x7f, b[off+1:off+n]
		switch typ {
		case subCreationTime, subExpirationTime:
			if len(data) != 4 {
				return errors.New("bad time subpacket")
			}
			if typ == subCreationTime {
				created = binary.BigEndian.Uint32(data)
			} else {
				expires = binary.BigEndian.Uint32(data)
			}
		case subIssuer, subIssuerFP:
		default:
			if b[off]&0x80 != 0 {
				return fmt.Errorf("unknown critical subpacket %d", typ)
			}
		}
		b = b[off+n:]
	}
	if expires != 0 && now.Unix() >= int64(created)+int64(expires) {
		return errors.New("expired")
	}
	return nil
}

// mpi reads one MPI of at most 32 bytes and returns it left-padded to 32.
func mpi(b []byte) ([]byte, []byte, bool) {
	if len(b) < 2 {
		return nil, nil, false
	}
	n := (int(binary.BigEndian.Uint16(b)) + 7) / 8
	if n > 32 || len(b) < 2+n {
		return nil, nil, false
	}
	v := make([]byte, 32)
	copy(v[32-n:], b[2:2+n])
	return v, b[2+n:], true
}

// crc24 is the OpenPGP armor checksum.
func crc24(b []byte) uint32 {
	crc := uint32(0xb704ce)
	for _, c := range b {
		crc ^= uint32(c) << 16
		for range 8 {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= 0x1864cfb
			}
		}
	}
	return crc & 0xffffff
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
