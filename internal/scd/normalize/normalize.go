package normalize

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/xtls/xray-core/common/uuid"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/country"
)

// Warning codes of an accepted link.
const (
	// WarnFlowCleared: a vision flow on a transport/security where Xray cannot
	// run it was dropped (D16).
	WarnFlowCleared = "flow_cleared"
	// WarnAllowInsecureIgnored: allowInsecure=1 without pcs/vcn was accepted
	// under the "allow" policy and is not passed to Xray (D15).
	WarnAllowInsecureIgnored = "allow_insecure_ignored"
	// WarnParamIgnored: the link has unknown parameters (D28).
	WarnParamIgnored = "param_ignored"
)

// Policy holds the per-subscription options that change normalization.
type Policy struct {
	// AllowInsecure accepts TLS links with allowInsecure=1 and neither pcs
	// nor vcn (subscriptions[].allowInsecure: allow, D15); otherwise they are
	// skipped.
	AllowInsecure bool
}

// Warning is a non-fatal note about an accepted link.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Params is the ordered query of a link; it marshals as a JSON object with
// the keys in link order.
type Params []vless.Param

// MarshalJSON implements json.Marshaler.
func (p Params) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(kv.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Normalized is a VLESS link resolved for Xray v26.10.21 (01 §1.2
// NormalizedOutbound plus the D26/D33 fields after shortId).
type Normalized struct {
	Kind     string `json:"kind"`
	Protocol string `json:"protocol"`
	Profile  string `json:"profile"`
	Tag      string `json:"tag"`
	Line     int    `json:"line"`
	Raw      string `json:"raw"`
	Label    string `json:"label"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	// UUID is the user id as canonicalized by Xray (D18).
	UUID string `json:"uuid"`
	// Encryption is "none" or the ML-KEM client string as in the link.
	Encryption string `json:"encryption"`
	// Network is tcp, ws or xhttp.
	Network string `json:"network"`
	// Security is tls, reality or none (none only with ML-KEM).
	Security string `json:"security"`
	Flow     string `json:"flow,omitempty"`
	// ALPN is never nil for a normalized link.
	ALPN        []string `json:"alpn"`
	SNI         string   `json:"sni,omitempty"`
	Host        string   `json:"host,omitempty"`
	Path        string   `json:"path,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	// AllowInsecure is the parsed TLS allowInsecure parameter; informational,
	// it never reaches Xray (D15).
	AllowInsecure        *bool           `json:"allowInsecure,omitempty"`
	HeaderType           *string         `json:"headerType,omitempty"`
	PublicKey            string          `json:"publicKey,omitempty"`
	ShortID              string          `json:"shortId,omitempty"`
	SpiderX              string          `json:"spiderX,omitempty"`
	Mldsa65Verify        string          `json:"mldsa65Verify,omitempty"`
	PinnedPeerCertSha256 []string        `json:"pinnedPeerCertSha256,omitempty"`
	VerifyPeerCertByName []string        `json:"verifyPeerCertByName,omitempty"`
	ECHConfigList        string          `json:"echConfigList,omitempty"`
	CipherSuites         string          `json:"cipherSuites,omitempty"`
	HeartbeatPeriod      uint32          `json:"heartbeatPeriod,omitempty"`
	XHTTPMode            string          `json:"xhttpMode,omitempty"`
	XHTTPExtra           json.RawMessage `json:"xhttpExtra,omitempty"`
	FinalMask            json.RawMessage `json:"finalmask,omitempty"`
	Country              *country.Info   `json:"country,omitempty"`
	City                 string          `json:"city,omitempty"`
	// Query holds all known parameters of the link, applicable or not.
	Query Params `json:"query"`
}

// Skip reasons of the normalizer (verbatim, 4.3).
const (
	reasonEncryption    = "Unsupported VLESS encryption value."
	reasonEncryptionKey = "Invalid VLESS encryption key."
	reasonUserID        = "The VLESS user id is not accepted by Xray."
	reasonFlow          = "Unsupported VLESS flow value."
	reasonAllowInsecure = "allowInsecure must be 0, 1, true, or false."
	reasonInsecureDeny  = "allowInsecure=1 requires pcs or vcn: Xray v26.10.21 no longer supports allowInsecure " +
		"(set subscriptions[].allowInsecure: allow to keep such links)."
	messageInsecure = "allowInsecure is not supported by Xray v26.10.21; the outbound will verify certificates (use pcs/vcn)."
)

const (
	flowVision       = "xtls-rprx-vision"
	flowVisionUDP443 = "xtls-rprx-vision-udp443"
)

// Normalize checks a parsed link against the D26 scope and the Xray v26.10.21
// rules that conf.Build cannot check (11 §5) and resolves its profile, tag and
// fields. A link is either an entry (with optional warnings) or a skip.
func Normalize(l vless.Link, p Policy) (Normalized, []Warning, *vless.Skip) {
	label := l.Label
	skip := func(code, reason string, details ...string) (Normalized, []Warning, *vless.Skip) {
		return Normalized{}, nil, &vless.Skip{
			Line: l.Line, Raw: l.Raw, Label: &label, Code: code, Reason: reason, Details: details,
		}
	}
	get := func(key string) string {
		v, _ := l.Get(key)
		return v
	}

	enc := get("encryption")
	mlkem := isMLKEM(enc)
	sc, ok := resolveScope(l, mlkem)
	if !ok {
		return skip(vless.CodeUnsupportedCombo, reasonCombo, comboDetails(l)...)
	}

	switch {
	case enc == "":
		enc = "none"
	case enc == "none":
	case mlkem:
		if err := validateMLKEM(enc); err != nil {
			return skip(vless.CodeUnsupportedValue, reasonEncryptionKey, "encryption="+truncate(enc, 40))
		}
	default:
		return skip(vless.CodeUnsupportedValue, reasonEncryption, "encryption="+enc)
	}

	id, err := uuid.ParseString(l.ID)
	if err != nil {
		return skip(vless.CodeUnsupportedValue, reasonUserID)
	}

	var warnings []Warning
	flow := get("flow")
	switch flow {
	case "", flowVision, flowVisionUDP443:
	default:
		return skip(vless.CodeUnsupportedValue, reasonFlow, "flow="+flow)
	}
	if flow != "" && !visionAllowed(sc, mlkem) {
		warnings = append(warnings, Warning{
			Code:    WarnFlowCleared,
			Message: "flow " + flow + " is not supported on " + sc.network + "+" + sc.security + "; flow cleared.",
		})
		flow = ""
	}

	n := Normalized{
		Kind:       "outbound",
		Protocol:   "vless",
		Profile:    sc.profile(flow),
		Line:       l.Line,
		Raw:        l.Raw,
		Label:      l.Label,
		Address:    l.Address,
		Port:       l.Port,
		UUID:       id.String(),
		Encryption: enc,
		Network:    sc.network,
		Security:   sc.security,
		Flow:       flow,
		ALPN:       []string{},
		Query:      Params(l.Params),
	}

	// allowInsecure is a TLS parameter: with reality or ML-KEM over none it
	// is inapplicable and dropped silently like any other (O16).
	if v, ok := l.Get("allowinsecure"); ok && sc.security == "tls" {
		var insecure bool
		switch strings.ToLower(v) {
		case "1", "true":
			insecure = true
		case "0", "false":
		default:
			return skip(vless.CodeUnsupportedValue, reasonAllowInsecure, "allowInsecure="+v)
		}
		n.AllowInsecure = &insecure
		if insecure && splitList(get("pcs")) == nil && splitList(get("vcn")) == nil {
			if !p.AllowInsecure {
				return skip(vless.CodeUnsupportedValue, reasonInsecureDeny)
			}
			warnings = append(warnings, Warning{Code: WarnAllowInsecureIgnored, Message: messageInsecure})
		}
	}

	if s := applySecurity(l, &n); s != nil {
		return Normalized{}, nil, s
	}
	if s := applyTransport(l, &n); s != nil {
		return Normalized{}, nil, s
	}

	n.Tag = BuildTag(l.Label, l.Address, n.Profile, l.Raw)
	if info, ok := country.FromLabel(l.Label); ok {
		n.Country = &info
	}
	n.City, _ = InferCity(l.Label)

	if len(l.Ignored) > 0 {
		warnings = append(warnings, Warning{
			Code:    WarnParamIgnored,
			Message: "Ignored unknown VLESS parameter(s): " + strings.Join(l.Ignored, ", ") + ".",
		})
	}
	return n, warnings, nil
}

// visionAllowed reports whether Xray runs a vision flow on sc: directly over
// TLS/REALITY on raw TCP, or inside VLESS Encryption on any transport (11 §3
// #2, #4, #12, #13).
func visionAllowed(sc scope, mlkem bool) bool {
	return mlkem || (sc.network == "tcp" && (sc.security == "tls" || sc.security == "reality"))
}

// truncate cuts s to n runes, marking a cut with "…".
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
