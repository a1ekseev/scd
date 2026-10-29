package outbound

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

// redacted is how a secret key is printed.
const redacted = "<redacted>"

// Secret is a secret key. Its JSON, text, fmt and slog forms are all
// "<redacted>"; only ToProto sees the value.
type Secret string

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// String implements fmt.Stringer.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter for every verb.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// WireGuard is a WireGuard outbound in the Xray v26.10.21 config form. The
// field order of every struct is the key order of its JSON.
type WireGuard struct {
	Tag            string            `json:"tag"`
	Protocol       string            `json:"protocol"`
	Settings       WireGuardSettings `json:"settings"`
	StreamSettings WireGuardStream   `json:"streamSettings"`
}

// WireGuardSettings is the WireGuard outbound "settings" object.
type WireGuardSettings struct {
	SecretKey Secret          `json:"secretKey"`
	Address   []string        `json:"address"`
	Peers     []WireGuardPeer `json:"peers"`
	MTU       int             `json:"mtu"`
	// Reserved is an array: a []byte would marshal as base64.
	Reserved []int `json:"reserved,omitempty"`
	// NoKernelTun is always true: a kernel TUN would change the routes and
	// policy rules of the host.
	NoKernelTun bool     `json:"noKernelTun"`
	RemoteDNS   []string `json:"remoteDNS,omitempty"`
}

// WireGuardPeer is the one peer of a WireGuard outbound.
type WireGuardPeer struct {
	PublicKey    string   `json:"publicKey"`
	PreSharedKey Secret   `json:"preSharedKey,omitempty"`
	Endpoint     string   `json:"endpoint"`
	KeepAlive    int      `json:"keepAlive,omitempty"`
	AllowedIPs   []string `json:"allowedIPs"`
}

// WireGuardStream is the "streamSettings" of a WireGuard outbound: only the
// endpoint domain strategy (without it Xray picks a random A or AAAA).
type WireGuardStream struct {
	Sockopt Sockopt `json:"sockopt"`
}

// Sockopt is "streamSettings.sockopt".
type Sockopt struct {
	DomainStrategy string `json:"domainStrategy"`
}

// BuildWireGuard returns the JSON outbound of candidate c under tag with the
// endpoint domain strategy.
func BuildWireGuard(c candidate.WireGuard, tag, domainStrategy string) WireGuard {
	strs := func(n int, f func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	s := WireGuardSettings{
		SecretKey:   Secret(base64.StdEncoding.EncodeToString(c.PrivateKey[:])),
		Address:     strs(len(c.Address), func(i int) string { return c.Address[i].String() }),
		MTU:         c.MTU,
		NoKernelTun: true,
	}
	if len(c.DNS) > 0 {
		s.RemoteDNS = strs(len(c.DNS), func(i int) string { return c.DNS[i].String() })
	}
	if r := c.Reserved; r != nil {
		s.Reserved = []int{int(r[0]), int(r[1]), int(r[2])}
	}
	p := WireGuardPeer{
		PublicKey:  base64.StdEncoding.EncodeToString(c.PublicKey[:]),
		Endpoint:   c.Endpoint,
		KeepAlive:  c.Keepalive,
		AllowedIPs: strs(len(c.AllowedIPs), func(i int) string { return c.AllowedIPs[i].String() }),
	}
	if c.PresharedKey != nil {
		p.PreSharedKey = Secret(base64.StdEncoding.EncodeToString(c.PresharedKey[:]))
	}
	s.Peers = []WireGuardPeer{p}
	return WireGuard{Tag: tag, Protocol: "wireguard", Settings: s, StreamSettings: WireGuardStream{Sockopt{domainStrategy}}}
}

// GetTag implements Spec.
func (w WireGuard) GetTag() string { return w.Tag }

// WithTag implements Spec.
func (w WireGuard) WithTag(tag string) Spec {
	w.Tag = tag
	return w
}

// xrayJSON is the JSON with the secret keys in place of "<redacted>".
func (w WireGuard) xrayJSON() ([]byte, error) {
	type plainPeer struct {
		WireGuardPeer
		PreSharedKey string `json:"preSharedKey,omitempty"`
	}
	type plainSettings struct {
		WireGuardSettings
		SecretKey string      `json:"secretKey"`
		Peers     []plainPeer `json:"peers"`
	}
	peers := make([]plainPeer, 0, len(w.Settings.Peers))
	for _, p := range w.Settings.Peers {
		peers = append(peers, plainPeer{p, string(p.PreSharedKey)})
	}
	return json.Marshal(struct {
		WireGuard
		Settings plainSettings `json:"settings"`
	}{w, plainSettings{w.Settings, string(w.Settings.SecretKey), peers}})
}
