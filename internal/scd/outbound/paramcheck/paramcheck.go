// Package paramcheck checks that a VLESS link parameter reached the protobuf
// outbound handler built from it (D6, D19, D26): each check reads the proto
// field the parameter maps to (11 §4), so a parameter dropped or changed on
// the way link → JSON → conf.Build fails even when the handler is otherwise
// valid. It is used by the tests of the outbound builder and the live tests.
package paramcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/vless"
	vout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/headers/http"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/splithttp"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Covered lists the link parameters (lowercase keys) that Check can verify
// (D33: 22 parameters).
var Covered = []string{
	"type", "security", "encryption", "flow", "sni", "fp", "alpn", "pcs", "vcn", "ech", "cs",
	"pbk", "sid", "spx", "pqv", "headertype", "host", "path", "heartbeatperiod", "mode", "extra", "fm",
}

// checks maps each covered parameter to its proto check.
var checks = map[string]func(value string, h *handler) error{
	"type":            checkType,
	"security":        checkSecurity,
	"encryption":      checkEncryption,
	"flow":            checkFlow,
	"sni":             checkSNI,
	"fp":              checkFingerprint,
	"alpn":            checkALPN,
	"pcs":             checkPCS,
	"vcn":             checkVCN,
	"ech":             checkECH,
	"cs":              checkCipherSuites,
	"pbk":             checkPbk,
	"sid":             checkSid,
	"spx":             checkSpx,
	"pqv":             checkPqv,
	"headertype":      checkHeaderType,
	"host":            checkHost,
	"path":            checkPath,
	"heartbeatperiod": checkHeartbeat,
	"mode":            checkMode,
	"extra":           checkExtra,
	"fm":              checkFinalMask,
}

// Check reports whether the link parameter param with value (as decoded from
// the link) is present in h. It returns an error naming the mismatching
// field, or an error for a parameter outside Covered.
func Check(param, value string, h *core.OutboundHandlerConfig) error {
	check, ok := checks[strings.ToLower(param)]
	if !ok {
		return fmt.Errorf("no check for parameter %q", param)
	}
	u, err := unpack(h)
	if err != nil {
		return err
	}
	return check(value, u)
}

// handler holds the unpacked messages of an outbound handler config.
type handler struct {
	stream  *internet.StreamConfig
	account *vless.Account
	tls     *tls.Config
	reality *reality.Config
	tcp     *tcp.Config
	ws      *websocket.Config
	xhttp   *splithttp.Config
}

func unpack(h *core.OutboundHandlerConfig) (*handler, error) {
	if h == nil || h.SenderSettings == nil || h.ProxySettings == nil {
		return nil, errors.New("incomplete outbound handler")
	}
	snd, err := h.SenderSettings.GetInstance()
	if err != nil {
		return nil, err
	}
	sender, ok := snd.(*proxyman.SenderConfig)
	if !ok || sender.StreamSettings == nil {
		return nil, errors.New("no stream settings")
	}
	prx, err := h.ProxySettings.GetInstance()
	if err != nil {
		return nil, err
	}
	vc, ok := prx.(*vout.Config)
	if !ok || vc.Vnext == nil || vc.Vnext.User == nil || vc.Vnext.User.Account == nil {
		return nil, errors.New("not a VLESS outbound")
	}
	acc, err := vc.Vnext.User.Account.GetInstance()
	if err != nil {
		return nil, err
	}
	u := &handler{stream: sender.StreamSettings}
	if u.account, ok = acc.(*vless.Account); !ok {
		return nil, errors.New("not a VLESS account")
	}
	for _, s := range u.stream.SecuritySettings {
		m, err := s.GetInstance()
		if err != nil {
			return nil, err
		}
		switch c := m.(type) {
		case *tls.Config:
			u.tls = c
		case *reality.Config:
			u.reality = c
		}
	}
	for _, ts := range u.stream.TransportSettings {
		m, err := ts.Settings.GetInstance()
		if err != nil {
			return nil, err
		}
		switch c := m.(type) {
		case *tcp.Config:
			u.tcp = c
		case *websocket.Config:
			u.ws = c
		case *splithttp.Config:
			u.xhttp = c
		}
	}
	return u, nil
}

// mismatch formats a field mismatch.
func mismatch(field string, got, want any) error {
	return fmt.Errorf("%s = %v, want %v", field, got, want)
}

func checkType(value string, h *handler) error {
	want, ok := map[string]string{"tcp": "tcp", "raw": "tcp", "ws": "websocket", "xhttp": "splithttp"}[value]
	if !ok {
		return fmt.Errorf("type %q is outside the D26 scope", value)
	}
	if h.stream.ProtocolName != want {
		return mismatch("StreamConfig.ProtocolName", h.stream.ProtocolName, want)
	}
	return nil
}

func checkSecurity(value string, h *handler) error {
	switch value {
	case "tls":
		if h.tls == nil || h.stream.SecurityType != serial.GetMessageType(h.tls) {
			return mismatch("StreamConfig.SecurityType", h.stream.SecurityType, "tls.Config")
		}
	case "reality":
		if h.reality == nil || h.stream.SecurityType != serial.GetMessageType(h.reality) {
			return mismatch("StreamConfig.SecurityType", h.stream.SecurityType, "reality.Config")
		}
	case "", "none":
		if h.stream.SecurityType != "" || len(h.stream.SecuritySettings) != 0 {
			return mismatch("StreamConfig.SecurityType", h.stream.SecurityType, `""`)
		}
	default:
		return fmt.Errorf("security %q is outside the D26 scope", value)
	}
	return nil
}

// checkEncryption checks the account against the ML-KEM transformation of
// conf (infra/conf/vless.go): the prefix mlkem768x25519plus.<mode>.<rtt> goes
// to XorMode/Seconds, the leading segments shorter than 20 characters to
// Padding and the keys to Encryption.
func checkEncryption(value string, h *handler) error {
	a := h.account
	if value == "" || value == "none" {
		if a.Encryption != "none" || a.XorMode != 0 || a.Seconds != 0 || a.Padding != "" {
			return mismatch("Account{Encryption,XorMode,Seconds,Padding}",
				[]any{a.Encryption, a.XorMode, a.Seconds, a.Padding}, []any{"none", 0, 0, ""})
		}
		return nil
	}
	s := strings.Split(value, ".")
	if len(s) < 4 || s[0] != "mlkem768x25519plus" {
		return fmt.Errorf("encryption %q is not an ML-KEM client string", value)
	}
	xor, ok := map[string]uint32{"native": 0, "xorpub": 1, "random": 2}[s[1]]
	if !ok {
		return fmt.Errorf("unknown ML-KEM mode %q", s[1])
	}
	var seconds uint32
	switch s[2] {
	case "1rtt":
	case "0rtt":
		seconds = 1
	default:
		return fmt.Errorf("unknown ML-KEM rtt %q", s[2])
	}
	rest := s[3:]
	i := 0
	for i < len(rest) && len(rest[i]) < 20 {
		i++
	}
	padding, keys := strings.Join(rest[:i], "."), strings.Join(rest[i:], ".")
	if a.XorMode != xor || a.Seconds != seconds || a.Padding != padding || a.Encryption != keys {
		return mismatch("Account{XorMode,Seconds,Padding,Encryption}",
			[]any{a.XorMode, a.Seconds, a.Padding, a.Encryption}, []any{xor, seconds, padding, keys})
	}
	return nil
}

func checkFlow(value string, h *handler) error {
	if h.account.Flow != value {
		return mismatch("Account.Flow", h.account.Flow, value)
	}
	return nil
}

// security returns the server name and fingerprint of the TLS or REALITY
// settings.
func security(h *handler) (serverName, fingerprint string, err error) {
	switch {
	case h.tls != nil:
		return h.tls.ServerName, h.tls.Fingerprint, nil
	case h.reality != nil:
		return h.reality.ServerName, h.reality.Fingerprint, nil
	}
	return "", "", errors.New("no TLS or REALITY settings")
}

func checkSNI(value string, h *handler) error {
	sn, _, err := security(h)
	if err != nil {
		return err
	}
	if sn != value {
		return mismatch("ServerName", sn, value)
	}
	return nil
}

func checkFingerprint(value string, h *handler) error {
	_, fp, err := security(h)
	if err != nil {
		return err
	}
	if want := strings.ToLower(value); fp != want {
		return mismatch("Fingerprint", fp, want)
	}
	return nil
}

func needTLS(h *handler) error {
	if h.tls == nil {
		return errors.New("no TLS settings")
	}
	return nil
}

func checkALPN(value string, h *handler) error {
	if err := needTLS(h); err != nil {
		return err
	}
	if want := splitList(value); !slices.Equal(h.tls.NextProtocol, want) {
		return mismatch("tls.Config.NextProtocol", h.tls.NextProtocol, want)
	}
	return nil
}

// checkPCS checks one 32-byte pin per pcs item (OpenSSL colon form allowed).
func checkPCS(value string, h *handler) error {
	if err := needTLS(h); err != nil {
		return err
	}
	items := splitList(value)
	got := h.tls.PinnedPeerCertSha256
	if len(got) != len(items) {
		return mismatch("len(tls.Config.PinnedPeerCertSha256)", len(got), len(items))
	}
	for i, item := range items {
		want, err := hex.DecodeString(strings.ReplaceAll(item, ":", ""))
		if err != nil {
			return fmt.Errorf("pcs item %q: %w", item, err)
		}
		if len(got[i]) != 32 || !bytes.Equal(got[i], want) {
			return mismatch(fmt.Sprintf("tls.Config.PinnedPeerCertSha256[%d]", i), hex.EncodeToString(got[i]), item)
		}
	}
	return nil
}

func checkVCN(value string, h *handler) error {
	if err := needTLS(h); err != nil {
		return err
	}
	if want := splitList(value); !slices.Equal(h.tls.VerifyPeerCertByName, want) {
		return mismatch("tls.Config.VerifyPeerCertByName", h.tls.VerifyPeerCertByName, want)
	}
	return nil
}

func checkECH(value string, h *handler) error {
	if err := needTLS(h); err != nil {
		return err
	}
	if h.tls.EchConfigList != value {
		return mismatch("tls.Config.EchConfigList", h.tls.EchConfigList, value)
	}
	return nil
}

func checkCipherSuites(value string, h *handler) error {
	if err := needTLS(h); err != nil {
		return err
	}
	if h.tls.CipherSuites != value {
		return mismatch("tls.Config.CipherSuites", h.tls.CipherSuites, value)
	}
	return nil
}

func needReality(h *handler) error {
	if h.reality == nil {
		return errors.New("no REALITY settings")
	}
	return nil
}

// pbkReplacer accepts a standard-base64 pbk as normalize does.
var pbkReplacer = strings.NewReplacer("=", "", "+", "-", "/", "_")

func checkPbk(value string, h *handler) error {
	if err := needReality(h); err != nil {
		return err
	}
	want, err := base64.RawURLEncoding.DecodeString(pbkReplacer.Replace(value))
	if err != nil {
		return fmt.Errorf("pbk %q: %w", value, err)
	}
	if got := h.reality.PublicKey; len(got) != 32 || !bytes.Equal(got, want) {
		return mismatch("reality.Config.PublicKey", base64.RawURLEncoding.EncodeToString(got), value)
	}
	return nil
}

// checkSid checks the 8-byte short id: the hex value padded with zero bytes.
func checkSid(value string, h *handler) error {
	if err := needReality(h); err != nil {
		return err
	}
	want := make([]byte, 8)
	if len(value) > 16 {
		return fmt.Errorf("sid %q is longer than 16 characters", value)
	}
	if _, err := hex.Decode(want, []byte(value)); err != nil {
		return fmt.Errorf("sid %q: %w", value, err)
	}
	if !bytes.Equal(h.reality.ShortId, want) {
		return mismatch("reality.Config.ShortId", hex.EncodeToString(h.reality.ShortId), hex.EncodeToString(want))
	}
	return nil
}

// checkSpx checks SpiderX without the p, c, t, i, r query parameters and
// SpiderY filled from them, as conf does (infra/conf/transport_security.go).
func checkSpx(value string, h *handler) error {
	if err := needReality(h); err != nil {
		return err
	}
	if value == "" {
		value = "/"
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("spx %q: %w", value, err)
	}
	q := u.Query()
	spiderY := make([]int64, 10)
	for i, key := range []string{"p", "c", "t", "i", "r"} {
		if v := q.Get(key); v != "" {
			lo, hi, found := strings.Cut(v, "-")
			if !found {
				hi = lo
			}
			spiderY[2*i], _ = strconv.ParseInt(lo, 10, 64)
			spiderY[2*i+1], _ = strconv.ParseInt(hi, 10, 64)
		}
		q.Del(key)
	}
	u.RawQuery = q.Encode()
	if got := h.reality.SpiderX; got != u.String() {
		return mismatch("reality.Config.SpiderX", got, u.String())
	}
	if !slices.Equal(h.reality.SpiderY, spiderY) {
		return mismatch("reality.Config.SpiderY", h.reality.SpiderY, spiderY)
	}
	return nil
}

func checkPqv(value string, h *handler) error {
	if err := needReality(h); err != nil {
		return err
	}
	want, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return fmt.Errorf("pqv: %w", err)
	}
	if got := h.reality.Mldsa65Verify; len(got) != 1952 || !bytes.Equal(got, want) {
		return mismatch("len(reality.Config.Mldsa65Verify)", len(got), len(want))
	}
	return nil
}

// httpHeader returns the http header request of a raw outbound.
func httpHeader(h *handler) (*http.RequestConfig, error) {
	if h.tcp == nil || h.tcp.HeaderSettings == nil {
		return nil, errors.New("no tcp header settings")
	}
	m, err := h.tcp.HeaderSettings.GetInstance()
	if err != nil {
		return nil, err
	}
	c, ok := m.(*http.Config)
	if !ok || c.Request == nil {
		return nil, mismatch("tcp.Config.HeaderSettings.Type", h.tcp.HeaderSettings.Type, serial.GetMessageType(&http.Config{}))
	}
	return c.Request, nil
}

// checkHeaderType: "none" (or empty) means no header settings; "http" means
// an http header; other types never reach an accepted outbound.
func checkHeaderType(value string, h *handler) error {
	if h.stream.ProtocolName != "tcp" {
		return mismatch("StreamConfig.ProtocolName", h.stream.ProtocolName, "tcp")
	}
	switch value {
	case "", "none":
		if h.tcp != nil && h.tcp.HeaderSettings != nil {
			return mismatch("tcp.Config.HeaderSettings.Type", h.tcp.HeaderSettings.Type, "none")
		}
		return nil
	case "http":
		_, err := httpHeader(h)
		return err
	}
	return fmt.Errorf("headerType %q is not accepted by Xray", value)
}

func checkHost(value string, h *handler) error {
	switch {
	case h.ws != nil:
		if h.ws.Host != value {
			return mismatch("websocket.Config.Host", h.ws.Host, value)
		}
	case h.xhttp != nil:
		if h.xhttp.Host != value {
			return mismatch("splithttp.Config.Host", h.xhttp.Host, value)
		}
	default:
		req, err := httpHeader(h)
		if err != nil {
			return err
		}
		want := splitList(value)
		if want == nil {
			return nil // Xray's default Host header list
		}
		for _, hd := range req.Header {
			if hd.Name == "Host" {
				if !slices.Equal(hd.Value, want) {
					return mismatch("http.RequestConfig Host", hd.Value, want)
				}
				return nil
			}
		}
		return errors.New("http.RequestConfig has no Host header")
	}
	return nil
}

// checkPath: ws path with ?ed=N split into Path and Ed as conf does; xhttp
// path as is; tcp http header path list (Xray's default "/" when empty).
func checkPath(value string, h *handler) error {
	switch {
	case h.ws != nil:
		path, ed := value, uint32(0)
		if u, err := url.Parse(value); err == nil {
			if q := u.Query(); q.Get("ed") != "" {
				n, _ := strconv.Atoi(q.Get("ed"))
				ed = uint32(n)
				q.Del("ed")
				u.RawQuery = q.Encode()
				path = u.String()
			}
		}
		if h.ws.Path != path || h.ws.Ed != ed {
			return mismatch("websocket.Config{Path,Ed}", []any{h.ws.Path, h.ws.Ed}, []any{path, ed})
		}
	case h.xhttp != nil:
		if h.xhttp.Path != value {
			return mismatch("splithttp.Config.Path", h.xhttp.Path, value)
		}
	default:
		req, err := httpHeader(h)
		if err != nil {
			return err
		}
		want := splitList(value)
		if want == nil {
			want = []string{"/"}
		}
		if !slices.Equal(req.Uri, want) {
			return mismatch("http.RequestConfig.Uri", req.Uri, want)
		}
	}
	return nil
}

func checkHeartbeat(value string, h *handler) error {
	if h.ws == nil {
		return errors.New("no websocket settings")
	}
	want, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fmt.Errorf("heartbeatPeriod %q: %w", value, err)
	}
	if uint64(h.ws.HeartbeatPeriod) != want {
		return mismatch("websocket.Config.HeartbeatPeriod", h.ws.HeartbeatPeriod, want)
	}
	return nil
}

func checkMode(value string, h *handler) error {
	if h.xhttp == nil {
		return errors.New("no xhttp settings")
	}
	want := value
	if want == "" {
		want = "auto"
	}
	if h.xhttp.Mode != want {
		return mismatch("splithttp.Config.Mode", h.xhttp.Mode, want)
	}
	return nil
}

// checkExtra checks every top-level extra key that is a splithttp.Config
// field (host, path and mode are overridden by the link): the field must
// equal what conf builds from that key alone, and at least one of them must
// be set in the handler.
func checkExtra(value string, h *handler) error {
	if h.xhttp == nil {
		return errors.New("no xhttp settings")
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &extra); err != nil {
		return fmt.Errorf("extra: %w", err)
	}
	fields := h.xhttp.ProtoReflect().Descriptor().Fields()
	set := 0
	for key, raw := range extra {
		fd := fields.ByJSONName(key)
		if fd == nil || key == "host" || key == "path" || key == "mode" {
			continue
		}
		want, err := buildExtraField(h.xhttp.Mode, key, raw)
		if err != nil {
			return fmt.Errorf("extra.%s: %w", key, err)
		}
		if got, want := only(h.xhttp, fd), only(want, fd); !proto.Equal(got, want) {
			return mismatch("splithttp.Config."+string(fd.Name()), got, want)
		}
		if h.xhttp.ProtoReflect().Has(fd) {
			set++
		}
	}
	if set == 0 {
		return errors.New("no splithttp.Config field is set from extra")
	}
	return nil
}

// buildExtraField builds a splithttp.Config with conf from one extra key.
func buildExtraField(mode, key string, raw json.RawMessage) (*splithttp.Config, error) {
	b, err := json.Marshal(map[string]any{"mode": mode, key: raw})
	if err != nil {
		return nil, err
	}
	var c conf.SplitHTTPConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	m, err := c.Build()
	if err != nil {
		return nil, err
	}
	return m.(*splithttp.Config), nil
}

// only returns a copy of m with just the field fd.
func only(m *splithttp.Config, fd protoreflect.FieldDescriptor) *splithttp.Config {
	c := &splithttp.Config{}
	if m.ProtoReflect().Has(fd) {
		c.ProtoReflect().Set(fd, m.ProtoReflect().Get(fd))
	}
	return c
}

// checkFinalMask checks that the finalmask produced masks or QUIC params.
func checkFinalMask(_ string, h *handler) error {
	if len(h.stream.Tcpmasks)+len(h.stream.Udpmasks) == 0 && h.stream.QuicParams == nil {
		return errors.New("StreamConfig has no tcpmasks, udpmasks or quicParams")
	}
	return nil
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
