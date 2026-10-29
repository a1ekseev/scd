package pgp

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func readModulus(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../srp/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProtonKeyFingerprint(t *testing.T) {
	_, fp, err := parseKey(protonModulusKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(fp[:]); got != "248097092b458509c508dac0350585c4e9518f26" {
		t.Fatalf("fingerprint %s", got)
	}
}

func TestVerifyModulus(t *testing.T) {
	for _, name := range []string{"modulus-1.asc", "modulus-2.asc"} {
		text, err := VerifyModulus(readModulus(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n, err := base64.StdEncoding.DecodeString(text); err != nil || len(n) != 256 {
			t.Fatalf("%s: modulus %q: %v", name, text, err)
		}
	}
}

func TestVerifyModulusRejects(t *testing.T) {
	m1, m2 := readModulus(t, "modulus-1.asc"), readModulus(t, "modulus-2.asc")
	body := func(m string) string { return strings.Split(m, "\n")[3] }
	split := func(m string) (string, string) {
		a, b, _ := strings.Cut(m, beginSignature)
		return a, beginSignature + b
	}
	head1, _ := split(m1)
	_, sig2 := split(m2)
	own := sign(t, "own", signOpts{})
	if _, err := verify(own.msg, own.key); err != nil {
		t.Fatalf("own key: %v", err)
	}
	cases := map[string]string{
		"substituted modulus":   strings.Replace(m1, body(m1), body(m2), 1),
		"swapped signature":     head1 + sig2,
		"one changed character": strings.Replace(m1, "W2z5", "W2z6", 1),
		"data after signature":  m1 + "data after modulus",
		"blank line after":      m1 + "\n",
		"data before":           "x\n" + m1,
		"second message":        m1 + m2,
		"truncated":             m1[:len(m1)-40],
		"bad checksum":          strings.Replace(m1, "=q6vu", "=q6vv", 1),
		"bad hash header":       strings.Replace(m1, "Hash: SHA256", "Hush: SHA256", 1),
		"other key":             own.msg,
		"empty":                 "",
	}
	for name, msg := range cases {
		if _, err := VerifyModulus(msg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVerifyOwnKey(t *testing.T) {
	for _, h := range []crypto.Hash{crypto.SHA256, crypto.SHA512} {
		s := sign(t, "line one  \n- dashed\nlast", signOpts{hash: h})
		text, err := verify(s.msg, s.key)
		if err != nil {
			t.Fatalf("%v: %v", h, err)
		}
		if text != "line one\ndashed\nlast" {
			t.Fatalf("%v: text %q", h, text)
		}
		if _, err := verify(strings.Replace(s.msg, "last", "lost", 1), s.key); err == nil {
			t.Fatalf("%v: changed text accepted", h)
		}
	}
}

type signed struct {
	msg string
	key []byte
}

// sign makes a cleartext-signed message with a fresh Ed25519 key, the way
// OpenPGP implementations do, to test the parser on our own key.
// signOpts changes what sign produces; zero values give a valid SHA-256
// text signature.
type signOpts struct {
	hash   crypto.Hash
	hashID byte   // overrides the algorithm id in the packet
	binary bool   // signature type 0x00 instead of 0x01
	header string // overrides the Hash header value
	extra  []byte // more hashed subpackets
}

func sign(t *testing.T, text string, o signOpts) signed {
	t.Helper()
	if o.hash == 0 {
		o.hash = crypto.SHA256
	}
	ids := map[crypto.Hash]byte{crypto.SHA256: 8, crypto.SHA512: 10}
	names := map[crypto.Hash]string{crypto.SHA256: "SHA256", crypto.SHA512: "SHA512"}
	if o.hashID == 0 {
		o.hashID = ids[o.hash]
	}
	sigType := byte(sigText)
	if o.binary {
		sigType = 0x00
	}
	if o.header == "" {
		o.header = names[o.hash]
	}
	hash := o.hash
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kb := append([]byte{4, 0x5c, 0, 0, 0, algoEdDSA, byte(len(oidEd25519))}, oidEd25519...)
	kb = append(append(kb, 1, 7, 0x40), pub...)
	key := append([]byte{0xc0 | tagPublicKey, byte(len(kb))}, kb...)

	sub := append([]byte{5, 2, 0x5c, 0, 0, 0}, o.extra...)
	hashed := append([]byte{4, sigType, algoEdDSA, o.hashID, 0, byte(len(sub))}, sub...)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(strings.TrimPrefix(l, "- "), " \t")
	}
	h := hash.New()
	h.Write([]byte(strings.Join(lines, "\r\n")))
	h.Write(hashed)
	h.Write([]byte{4, 0xff})
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(hashed))))
	digest := h.Sum(nil)
	sig := ed25519.Sign(priv, digest)
	sb := append(append(hashed, 0, 0), digest[:2]...)
	sb = append(append(append(sb, 1, 0), sig[:32]...), append([]byte{1, 0}, sig[32:]...)...)
	pkt := append([]byte{0xc0 | tagSignature, byte(len(sb))}, sb...)
	crc := crc24(pkt)
	msg := beginMessage + "\nHash: " + o.header + "\n\n" + text + "\n" + beginSignature + "\n\n" +
		base64.StdEncoding.EncodeToString(pkt) + "\n=" +
		base64.StdEncoding.EncodeToString([]byte{byte(crc >> 16), byte(crc >> 8), byte(crc)}) + "\n" + endSignature + "\n"
	return signed{msg: msg, key: key}
}

func TestVerifyRejectsSignatureShape(t *testing.T) {
	future := binary.BigEndian.AppendUint32([]byte{5, 3}, 0x7fffffff)
	cases := map[string]struct {
		text string
		o    signOpts
	}{
		"SHA-1 algorithm":         {"m", signOpts{hashID: 2}},
		"binary signature type":   {"m", signOpts{binary: true}},
		"MD5 header":              {"m", signOpts{header: "MD5"}},
		"mismatched Hash header":  {"m", signOpts{hash: crypto.SHA512, header: "SHA256"}},
		"undashed dash line":      {"-m", signOpts{}},
		"unknown critical sub":    {"m", signOpts{extra: []byte{2, 0x80 | 100, 0}}},
		"expired":                 {"m", signOpts{extra: []byte{5, 3, 0, 0, 0, 1}}},
		"bad expiration length":   {"m", signOpts{extra: []byte{2, 3, 0}}},
		"subpacket past the area": {"m", signOpts{extra: []byte{9, 3}}},
	}
	for name, c := range cases {
		s := sign(t, c.text, c.o)
		if _, err := verify(s.msg, s.key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, o := range map[string]signOpts{
		"unknown non-critical sub": {extra: []byte{2, 100, 0}},
		"future expiration":        {extra: future},
		"both hashes declared":     {header: "SHA256, SHA512"},
	} {
		s := sign(t, "m", o)
		if _, err := verify(s.msg, s.key); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
