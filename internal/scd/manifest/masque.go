package manifest

import (
	"crypto/sha256"
	"crypto/x509"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/selection"
)

// masqueEntry is the entry of masque candidate c. Its selection key is the
// SHA-256 of the PKIX device public key, so the h3 and h2 lines of one
// device share it.
func masqueEntry(c candidate.Masque, line int, tagPrefix string) Entry {
	ep := c.Endpoint.String()
	e := newCandidateEntry(ProtocolMasque, "mq", c.Label, c.Meta.Exit, ep, c.Endpoint.Addr().String(), int(c.Endpoint.Port()),
		line, tagPrefix, c.PublicHash(), c.Redacted())
	ob := outbound.BuildMasque(c, e.Tag)
	e.JSONOutbound = ob
	n := &e.Normalized
	n.Network, n.Security, n.SNI = "masque", "tls", c.SNI
	if alpn := ob.StreamSettings.TLSSettings.ALPN; alpn != nil {
		n.ALPN = alpn
	}
	pkix, err := x509.MarshalPKIXPublicKey(&c.PrivateKey.PublicKey)
	if err != nil {
		panic(err) // unreachable: ParseMasque returns P-256 keys only
	}
	e.selection = &selection.Candidate{
		Tag: e.Tag, Label: c.Label, Entry: c.Meta.Entry, Exit: c.Meta.Exit, Load: c.Meta.Load, Score: c.Meta.Score,
		Key: sha256.Sum256(pkix), Endpoint: ep, Transport: c.Transport, Priority: c.Priority,
	}
	return e
}
