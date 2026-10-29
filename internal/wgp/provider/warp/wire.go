// Package warp is the wgp provider type "warp" (Cloudflare WARP): it
// registers devices once, checks them on every tick, rotates their keys and
// publishes, with provider=warp, one "wireguard://" line per device (tunnel
// wireguard) or one "masque://" line per device and transport (tunnel
// masque: the device is switched to a P-256 key right after registration).
// wire.go holds the TLS ClientHello, the header set and the JSON encoding
// wgp sends to the WARP API.
package warp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// apiHost is the WARP API host; the path starts with the API version.
const apiHost = "api.cloudflareclient.com"

// Identification is identification.warp: the app strings sent to the API.
type Identification struct {
	// UserAgent is the User-Agent header.
	UserAgent string `json:"userAgent"`
	// ClientVersion is the CF-Client-Version header.
	ClientVersion string `json:"clientVersion"`
	// APIVersion is the first path segment, for example v0a5641.
	APIVersion string `json:"apiVersion"`
	// Model, Locale and OSVersion go into the POST /reg body.
	Model     string `json:"model"`
	Locale    string `json:"locale"`
	OSVersion string `json:"osVersion"`
}

// helloSpec is the WARP API ClientHello: TLS 1.2 only, ECDHE AES-256-GCM
// suites c02c and c030, fixed extension order, no GREASE, 161 bytes for a
// fresh hello to apiHost.
func helloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS12,
		CipherSuites:       []uint16{utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateNever},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256, utls.PSSWithSHA256, utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384, utls.PSSWithSHA384, utls.PKCS1WithSHA384,
				utls.PSSWithSHA512, utls.PKCS1WithSHA512, utls.PKCS1WithSHA1,
			}},
			// BoringSSL padding: present only when the hello is 256..511
			// bytes, that is on resumption with a ticket.
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// prepareHello sends an empty session_id on a fresh hello and a random
// 32-byte one only when a session ticket is offered.
func prepareHello(c *utls.UConn) error {
	c.HandshakeState.Hello.SessionId = nil
	if err := c.BuildHandshakeState(); err != nil {
		return err
	}
	for _, e := range c.Extensions {
		if t, ok := e.(*utls.SessionTicketExtension); ok && len(t.Ticket) > 0 {
			c.HandshakeState.Hello.SessionId = make([]byte, 32)
			_, _ = rand.Read(c.HandshakeState.Hello.SessionId) // never fails
			return c.MarshalClientHello()
		}
	}
	return nil
}

// api sends WARP API requests.
type api struct {
	c  *transport.Client
	id Identification
}

// newAPI returns the WARP API client. cfg gives RootCAs, Dial and API; the
// base URL, mode and ClientHello are set here.
func newAPI(id Identification, cfg transport.Config) (*api, error) {
	cfg.BaseURL = "https://" + apiHost + "/" + id.APIVersion
	cfg.Mode = transport.HTTP
	cfg.Spec = helloSpec
	cfg.Prepare = prepareHello
	c, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	return &api{c: c, id: id}, nil
}

// do sends one request. token, if set, goes as a Bearer token; body, if
// set, as JSON with the keys sorted and a trailing newline. net/http orders
// the headers: Host, User-Agent, Content-Length, the rest sorted,
// Accept-Encoding last.
func (a *api) do(ctx context.Context, method, path, token string, body map[string]string) (transport.Response, error) {
	h := []transport.Header{
		{Name: "User-Agent", Value: a.id.UserAgent},
		{Name: "Cf-Client-Version", Value: a.id.ClientVersion}, // net/http canonical case
		{Name: "Connection", Value: "Keep-Alive"},
	}
	if token != "" {
		h = append(h, transport.Header{Name: "Authorization", Value: "Bearer " + token})
	}
	var b []byte
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return transport.Response{}, err
		}
		b = buf.Bytes()
		h = append(h, transport.Header{Name: "Content-Type", Value: "application/json; charset=UTF-8"})
	}
	return a.c.Do(ctx, transport.Request{Method: method, Path: path, Header: h, Body: b})
}

// regBody is the POST /reg body for the X25519 public key key.
func regBody(id Identification, key string, tos time.Time) map[string]string {
	return map[string]string{
		"fcm_token":     "",
		"install_id":    "",
		"key":           key,
		"key_type":      "curve25519",
		"locale":        id.Locale,
		"model":         id.Model,
		"os_version":    id.OSVersion,
		"serial_number": "",
		"tos":           tos.UTC().Format(time.RFC3339Nano),
		"tunnel_type":   "wireguard",
	}
}

// keyBody is the PATCH /reg/{id} body that sets a new device key.
func keyBody(key, keyType, tunnelType string) map[string]string {
	return map[string]string{"key": key, "key_type": keyType, "tunnel_type": tunnelType}
}
