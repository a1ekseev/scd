package outbound

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

// Masque is a MASQUE outbound in the Xray v26.10.21 config form: protocol
// and transport masque in the device mode of masqueSettings (the device key
// signs the client certificate, the endpoint key is pinned). The field
// order of every struct is the key order of its JSON.
type Masque struct {
	Tag            string         `json:"tag"`
	Protocol       string         `json:"protocol"`
	Settings       MasqueSettings `json:"settings"`
	StreamSettings MasqueStream   `json:"streamSettings"`
}

// MasqueSettings is the MASQUE outbound "settings" object.
type MasqueSettings struct {
	Address   string   `json:"address"`
	Port      int      `json:"port"`
	RemoteDNS []string `json:"remoteDNS,omitempty"`
}

// MasqueStream is the "streamSettings" of a MASQUE outbound. Security is
// always tls: Xray checks it only in AddOutbound, not in conf.Build.
type MasqueStream struct {
	Network        string          `json:"network"`
	MasqueSettings MasqueTransport `json:"masqueSettings"`
	Security       string          `json:"security"`
	TLSSettings    MasqueTLS       `json:"tlsSettings"`
}

// MasqueTransport is "masqueSettings": the device block only, so Xray sets
// the host and path of the device mode itself.
type MasqueTransport struct {
	Device MasqueDevice `json:"warp"`
}

// MasqueDevice is the device block of "masqueSettings".
type MasqueDevice struct {
	// PrivateKey is the std base64 SEC 1 DER of the device key.
	PrivateKey Secret `json:"privateKey"`
	// PublicKey is the PEM PKIX of the endpoint key.
	PublicKey string   `json:"publicKey"`
	Address   []string `json:"address"`
}

// MasqueTLS is "tlsSettings": the server name, and alpn ["h2"] for HTTP/2
// (HTTP/3 without it). No fingerprint and no pinnedPeerCertSha256: the
// latter would turn off the endpoint key pinning.
type MasqueTLS struct {
	ServerName string   `json:"serverName"`
	ALPN       []string `json:"alpn,omitempty"`
}

// BuildMasque returns the JSON outbound of candidate c under tag.
func BuildMasque(c candidate.Masque, tag string) Masque {
	sec1, err := x509.MarshalECPrivateKey(c.PrivateKey)
	if err != nil {
		panic(err) // unreachable: ParseMasque returns P-256 keys only
	}
	pkix, err := x509.MarshalPKIXPublicKey(c.PeerKey)
	if err != nil {
		panic(err) // unreachable: as above
	}
	m := Masque{Tag: tag, Protocol: "masque"}
	m.Settings = MasqueSettings{Address: c.Endpoint.Addr().String(), Port: int(c.Endpoint.Port())}
	for _, a := range c.DNS {
		m.Settings.RemoteDNS = append(m.Settings.RemoteDNS, a.String())
	}
	d := MasqueDevice{
		PrivateKey: Secret(base64.StdEncoding.EncodeToString(sec1)),
		PublicKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})),
		Address:    make([]string, 0, len(c.Address)),
	}
	for _, p := range c.Address {
		d.Address = append(d.Address, p.String())
	}
	m.StreamSettings = MasqueStream{
		Network: "masque", MasqueSettings: MasqueTransport{d}, Security: "tls",
		TLSSettings: MasqueTLS{ServerName: c.SNI},
	}
	if c.Transport == candidate.TransportH2 {
		m.StreamSettings.TLSSettings.ALPN = []string{"h2"}
	}
	return m
}

// GetTag implements Spec.
func (m Masque) GetTag() string { return m.Tag }

// WithTag implements Spec.
func (m Masque) WithTag(tag string) Spec {
	m.Tag = tag
	return m
}

// xrayJSON is the JSON with the device key in place of "<redacted>".
func (m Masque) xrayJSON() ([]byte, error) {
	type plainDevice struct {
		MasqueDevice
		PrivateKey string `json:"privateKey"`
	}
	type plainTransport struct {
		Device plainDevice `json:"warp"`
	}
	type plainStream struct {
		MasqueStream
		MasqueSettings plainTransport `json:"masqueSettings"`
	}
	d := m.StreamSettings.MasqueSettings.Device
	return json.Marshal(struct {
		Masque
		StreamSettings plainStream `json:"streamSettings"`
	}{m, plainStream{m.StreamSettings, plainTransport{plainDevice{d, string(d.PrivateKey)}}}})
}
