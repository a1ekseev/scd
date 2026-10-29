// Package proton is the wgp provider for Proton VPN. Proton API requests
// reproduce the TLS ClientHello, the HTTP headers and the JSON of the
// official Proton VPN Linux client (wire.go).
package proton

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	utls "github.com/refraction-networking/utls"

	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// apiHost is the Proton VPN API host.
const apiHost = "vpn-api.proton.me"

// Identification is identification.proton: the client strings sent to the
// API.
type Identification struct {
	// AppVersion is the x-pm-appversion header, for example
	// linux-vpn-gui@5.8.11+x86_64.
	AppVersion string `json:"appVersion"`
	// UserAgent is the User-Agent header.
	UserAgent string `json:"userAgent"`
	// Timezone is the x-pm-timezone header, an IANA name.
	Timezone string `json:"timezone"`
}

// helloSpec is the ClientHello of the official Proton Linux client: TLS 1.3
// and 1.2, no GREASE, padding or PSK, key shares X25519MLKEM768 and x25519
// with their own x25519 keys.
func helloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8, 0xccaa, 0xc02b,
			0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023, 0xc027, 0x0067, 0xc00a, 0xc014,
			0x0039, 0xc009, 0xc013, 0x0033, 0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateNever},
			&utls.SNIExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0, 1, 2}},
			// x448, ffdhe2048 and ffdhe3072 are only advertised: uTLS cannot
			// use them, and the server picks a TLS 1.3 group it supports.
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveID(30),
				utls.CurveP384, utls.CurveP521, utls.CurveID(256), utls.CurveID(257),
			}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.GenericExtension{Id: 22}, // encrypt_then_mac
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0905, 0x0906, 0x0904, 0x0403, 0x0503, 0x0603, 0x0807, 0x0808, 0x081a, 0x081b, 0x081c, 0x0809, 0x080a,
				0x080b, 0x0804, 0x0805, 0x0806, 0x0401, 0x0501, 0x0601, 0x0303, 0x0301, 0x0302, 0x0402, 0x0502, 0x0602,
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.UtlsCompressCertExtension{Algorithms: []utls.CertCompressionAlgo{utls.CertCompressionZlib, utls.CertCompressionZstd}},
		},
	}
}

// api sends Proton API requests: raw HTTP/1.1, one connection per request,
// closed without a TLS close_notify, as the client does.
type api struct {
	c  *transport.Client
	id Identification
}

// newAPI returns the Proton API client. cfg gives RootCAs, Dial and API; the
// base URL, mode and ClientHello are set here.
func newAPI(id Identification, cfg transport.Config) (*api, error) {
	cfg.BaseURL = "https://" + apiHost
	cfg.Mode = transport.Raw
	cfg.Spec = helloSpec
	c, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	return &api{c: c, id: id}, nil
}

// call is one API request.
type call struct {
	method, path string
	// uid and token are the session; empty before login.
	uid, token string
	// header goes after x-pm-timezone (X-PM-netzone, If-Modified-Since).
	header []transport.Header
	// body, if set, is sent as JSON in the json.dumps style.
	body obj
	// dialed is transport.Request.Dialed.
	dialed func()
}

// do sends c with the headers of the client in its order and case:
// x-pm-timezone on every request except ping, refresh and logout;
// Content-Length on every request but GET (0 without a body), Content-Type
// only with a body.
func (a *api) do(ctx context.Context, c call) (transport.Response, error) {
	h := []transport.Header{
		{Name: "Host", Value: apiHost},
		{Name: "x-pm-appversion", Value: a.id.AppVersion},
		{Name: "User-Agent", Value: a.id.UserAgent},
	}
	if c.uid != "" {
		h = append(h, transport.Header{Name: "x-pm-uid", Value: c.uid})
	}
	if c.token != "" {
		h = append(h, transport.Header{Name: "Authorization", Value: "Bearer " + c.token})
	}
	if sendsTimezone(c.method, c.path) {
		h = append(h, transport.Header{Name: "x-pm-timezone", Value: a.id.Timezone})
	}
	h = append(h, c.header...)
	h = append(h, transport.Header{Name: "Accept", Value: "*/*"}, transport.Header{Name: "Accept-Encoding", Value: "gzip, deflate, zstd"})
	var b []byte
	if c.body != nil {
		var err error
		if b, err = dumps(c.body); err != nil {
			return transport.Response{}, err
		}
	}
	if c.method != http.MethodGet {
		h = append(h, transport.Header{Name: "Content-Length", Value: strconv.Itoa(len(b))})
	}
	if c.body != nil {
		h = append(h, transport.Header{Name: "Content-Type", Value: "application/json"})
	}
	return a.c.Do(ctx, transport.Request{Method: c.method, Path: c.path, Header: h, Body: b, Dialed: c.dialed})
}

// sendsTimezone reports whether the client sends x-pm-timezone on a
// request: not on ping, refresh and logout.
func sendsTimezone(method, path string) bool {
	switch method + " " + path {
	case "GET /tests/ping", "POST /auth/refresh", "DELETE /auth":
		return false
	}
	return true
}

// obj is a JSON object with its keys in insertion order.
type obj []kv

// kv is one member of an obj. V is a string, bool, int, nil or obj.
type kv struct {
	K string
	V any
}

// dumps encodes v as Python json.dumps does by default: ", " and ": "
// separators, keys in insertion order, non-ASCII and control characters as
// \uXXXX (UTF-16 surrogate pairs above the BMP), no HTML escaping.
func dumps(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := dump(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func dump(b *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case string:
		return quote(b, v)
	case obj:
		b.WriteByte('{')
		for i, m := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := quote(b, m.K); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := dump(b, m.V); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("json: unsupported type %T", v)
	}
	return nil
}

// quote writes s as a JSON string with Python's ensure_ascii escaping.
func quote(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("json: invalid UTF-8 in a string")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
			default:
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}
