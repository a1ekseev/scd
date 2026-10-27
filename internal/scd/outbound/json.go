// Package outbound builds the Xray v26.10.21 JSON outbound of a normalized
// VLESS link and turns it into the protobuf handler config of the Xray API
// through infra/conf (D19: link → JSON outbound → conf.Build → protobuf).
//
// The JSON form is what scd generate prints and what the outbound hash is
// computed from (09 §6, 11 §4). Values are mapped, not validated: conf.Build
// rejects what Xray rejects (D26, 11 §5).
package outbound

import (
	"encoding/json"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/normalize"
)

// Outbound is a VLESS outbound in the Xray v26.10.21 config form. The field
// order of every struct is the key order of its JSON.
type Outbound struct {
	Tag            string         `json:"tag"`
	Protocol       string         `json:"protocol"`
	Settings       Settings       `json:"settings"`
	StreamSettings StreamSettings `json:"streamSettings"`
}

// Settings is the VLESS outbound "settings" object.
type Settings struct {
	Vnext []Server `json:"vnext"`
}

// Server is one "vnext" item.
type Server struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Users   []User `json:"users"`
}

// User is a VLESS user; Email is the outbound tag (as in TS).
type User struct {
	ID string `json:"id"`
	// Encryption is "none" or the ML-KEM client string as in the link; conf
	// splits it into the account fields.
	Encryption string `json:"encryption"`
	Flow       string `json:"flow,omitempty"`
	Email      string `json:"email"`
}

// StreamSettings is "streamSettings". Only the sections of the outbound's
// network and security are set.
type StreamSettings struct {
	Network         string           `json:"network"`
	Security        string           `json:"security"`
	TLSSettings     *TLSSettings     `json:"tlsSettings,omitempty"`
	RealitySettings *RealitySettings `json:"realitySettings,omitempty"`
	TCPSettings     *TCPSettings     `json:"tcpSettings,omitempty"`
	WSSettings      *WSSettings      `json:"wsSettings,omitempty"`
	XHTTPSettings   *XHTTPSettings   `json:"xhttpSettings,omitempty"`
	// FinalMask is the fm parameter, a JSON object embedded as is.
	FinalMask json.RawMessage `json:"finalmask,omitempty"`
}

// TLSSettings is "tlsSettings"; allowInsecure is never written (D15).
type TLSSettings struct {
	ServerName  string   `json:"serverName,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	// PinnedPeerCertSha256 is the comma-joined pcs list (conf splits it).
	PinnedPeerCertSha256 string `json:"pinnedPeerCertSha256,omitempty"`
	// VerifyPeerCertByName is the comma-joined vcn list (conf splits it).
	VerifyPeerCertByName string `json:"verifyPeerCertByName,omitempty"`
	ECHConfigList        string `json:"echConfigList,omitempty"`
	CipherSuites         string `json:"cipherSuites,omitempty"`
}

// RealitySettings is "realitySettings". SpiderX is always set, so conf
// always fills SpiderY (D13).
type RealitySettings struct {
	ServerName    string `json:"serverName,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Password      string `json:"password"`
	ShortID       string `json:"shortId"`
	SpiderX       string `json:"spiderX"`
	Mldsa65Verify string `json:"mldsa65Verify,omitempty"`
}

// TCPSettings is "tcpSettings" (raw transport header obfuscation).
type TCPSettings struct {
	Header TCPHeader `json:"header"`
}

// TCPHeader is the raw transport header. Request is set for type http only;
// any other type is passed to conf, which rejects unknown ones.
type TCPHeader struct {
	Type    string         `json:"type"`
	Request *HeaderRequest `json:"request,omitempty"`
}

// HeaderRequest is the http header request. Unset keys keep Xray's defaults;
// a set Headers replaces Xray's whole default header set (11 §4).
type HeaderRequest struct {
	Path    []string            `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// WSSettings is "wsSettings"; Path is as in the link (conf extracts ?ed=).
type WSSettings struct {
	Path            string `json:"path,omitempty"`
	Host            string `json:"host,omitempty"`
	HeartbeatPeriod uint32 `json:"heartbeatPeriod,omitempty"`
}

// XHTTPSettings is "xhttpSettings"; Extra is embedded as is.
type XHTTPSettings struct {
	Path  string          `json:"path,omitempty"`
	Host  string          `json:"host,omitempty"`
	Mode  string          `json:"mode,omitempty"`
	Extra json.RawMessage `json:"extra,omitempty"`
}

// Build returns the JSON outbound of n.
func Build(n normalize.Normalized) Outbound {
	return Outbound{
		Tag:      n.Tag,
		Protocol: "vless",
		Settings: Settings{Vnext: []Server{{
			Address: n.Address,
			Port:    n.Port,
			Users:   []User{{ID: n.UUID, Encryption: n.Encryption, Flow: n.Flow, Email: n.Tag}},
		}}},
		StreamSettings: buildStream(n),
	}
}

func buildStream(n normalize.Normalized) StreamSettings {
	s := StreamSettings{Network: n.Network, Security: n.Security, FinalMask: n.FinalMask}
	switch n.Security {
	case "tls":
		s.TLSSettings = &TLSSettings{
			ServerName:           n.SNI,
			ALPN:                 n.ALPN,
			Fingerprint:          n.Fingerprint,
			PinnedPeerCertSha256: strings.Join(n.PinnedPeerCertSha256, ","),
			VerifyPeerCertByName: strings.Join(n.VerifyPeerCertByName, ","),
			ECHConfigList:        n.ECHConfigList,
			CipherSuites:         n.CipherSuites,
		}
	case "reality":
		s.RealitySettings = &RealitySettings{
			ServerName:    n.SNI,
			Fingerprint:   n.Fingerprint,
			Password:      n.PublicKey,
			ShortID:       n.ShortID,
			SpiderX:       n.SpiderX,
			Mldsa65Verify: n.Mldsa65Verify,
		}
	}
	switch n.Network {
	case "tcp":
		s.TCPSettings = buildTCP(n)
	case "ws":
		s.WSSettings = &WSSettings{Path: n.Path, Host: n.Host, HeartbeatPeriod: n.HeartbeatPeriod}
	case "xhttp":
		s.XHTTPSettings = &XHTTPSettings{Path: n.Path, Host: n.Host, Mode: n.XHTTPMode, Extra: n.XHTTPExtra}
	}
	return s
}

// buildTCP returns the tcpSettings of a raw outbound: none without a
// headerType (or with "" / "none"), a request for http, and a bare header of
// any other type, which conf rejects (corpus line 107).
func buildTCP(n normalize.Normalized) *TCPSettings {
	if n.HeaderType == nil || *n.HeaderType == "" || *n.HeaderType == "none" {
		return nil
	}
	h := TCPHeader{Type: *n.HeaderType}
	if h.Type == "http" {
		req := &HeaderRequest{Path: splitList(n.Path)}
		if hosts := splitList(n.Host); hosts != nil {
			req.Headers = map[string][]string{"Host": hosts}
		}
		h.Request = req
	}
	return &TCPSettings{Header: h}
}

// splitList splits a comma-separated list, trimming items and dropping blank
// ones; nil when nothing is left.
func splitList(v string) []string {
	var items []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
