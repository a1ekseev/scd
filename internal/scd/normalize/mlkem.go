package normalize

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/proxy/vless/encryption"
)

// mlkemPrefix starts every VLESS Encryption (ML-KEM) client string.
const mlkemPrefix = "mlkem768x25519plus."

// mlkemPaddingLen: conf treats segments shorter than this as padding.
const mlkemPaddingLen = 20

func isMLKEM(enc string) bool { return strings.HasPrefix(enc, mlkemPrefix) }

// validateMLKEM checks the keys and padding of an ML-KEM client string
// "mlkem768x25519plus.<mode>.<rtt>.<segments>" (11 §4 encryption, D25):
//   - every segment of 20 or more characters must be base64url (no padding)
//     of an X25519 public key (32 bytes) or an ML-KEM-768 encapsulation key
//     (1184 bytes); conf.Build checks only the length, while the live Xray
//     rejects a key that does not decode;
//   - shorter segments are padding: conf.Build slices them off the front of
//     the key list (infra/conf/vless.go:357-371), so they must all precede
//     the first key, and together they must pass the live Xray's
//     encryption.ParsePadding (proxy/vless/encryption/common.go:223-257).
//
// Mode and rtt are left to conf.
func validateMLKEM(enc string) error {
	parts := strings.Split(enc, ".")
	if len(parts) < 4 {
		return nil // conf rejects it with its own message
	}
	keys := 0
	var padding []string
	for i, seg := range parts[3:] {
		if len(seg) < mlkemPaddingLen {
			if keys > 0 {
				return fmt.Errorf("encryption segment %d: padding after a key", i+4)
			}
			padding = append(padding, seg)
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(seg)
		if err == nil {
			switch len(b) {
			case 32:
				_, err = ecdh.X25519().NewPublicKey(b)
			case 1184:
				_, err = mlkem.NewEncapsulationKey768(b)
			default:
				err = fmt.Errorf("key of %d bytes", len(b))
			}
		}
		if err != nil {
			return fmt.Errorf("encryption segment %d: %w", i+4, err)
		}
		keys++
	}
	if keys == 0 {
		// conf.Build would slice past the end of a string of padding only.
		return errors.New("encryption has no key")
	}
	var lens, gaps [][3]int
	if err := encryption.ParsePadding(strings.Join(padding, "."), &lens, &gaps); err != nil {
		return fmt.Errorf("encryption padding: %w", err)
	}
	return nil
}
