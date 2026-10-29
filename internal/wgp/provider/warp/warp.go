package warp

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// Type is the provider type name.
const Type = "warp"

// Tunnels.
const (
	tunnelWireGuard = "wireguard"
	tunnelMasque    = "masque"
)

// WARP API error codes that, with their status, mean the device is gone.
const (
	codeNotFound      = 1006 // 404: registration not found
	codeNotAuthorized = 1012 // 401: token not accepted
)

var (
	// cloudflareDNS is the default wireguard.dns.
	cloudflareDNS = []string{"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001"}
	// wgPorts are the WireGuard ports of the WARP endpoint.
	wgPorts = []int{2408, 500, 1701, 4500}
	// idPattern is a device id: it is pasted into the request path.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// apiVersionPattern is identification.warp.apiVersion: a path segment.
	apiVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// WireGuard is the wireguard subtree: the tunnel values the API does not give.
type WireGuard struct {
	DNS        []string `json:"dns"`
	MTU        int      `json:"mtu"`
	AllowedIPs []string `json:"allowedIPs"`
	// Port replaces the endpoint port of the API; 0 keeps it.
	Port int `json:"port,omitzero"`
}

// Masque is the masque subtree: the tunnel values the API does not give.
type Masque struct {
	// Transports are the published transports; the index is the priority.
	Transports []string `json:"transports"`
	SNI        string   `json:"sni"`
	// Port replaces the port of the API endpoint, for h3 (UDP) and h2 (TCP).
	Port int      `json:"port"`
	DNS  []string `json:"dns"`
}

// WARP is one configured WARP provider: Candidates devices, each with its
// own key and its own line. Its JSON is its config keys.
type WARP struct {
	id    string
	out   provider.Output
	ident Identification
	// cfg is the transport config; tests set RootCAs and Dial.
	cfg transport.Config
	now func() time.Time
	api *api
	// rotate holds the device slots still to rotate in this process; nil
	// before the first tick.
	rotate map[int]bool
	// regErr is the error of the last failed POST /reg per slot in this
	// process: the ticks until the next attempt report the same text, so
	// it changes once per reRegisterMinIntervalMinutes.
	regErr map[int]string

	Tunnel string `json:"tunnel"`
	Keys   struct {
		RotateOnStart bool `json:"rotateOnStart"`
	} `json:"keys"`
	DeviceLostAfterChecks        int    `json:"deviceLostAfterChecks"`
	ReRegisterMinIntervalMinutes int    `json:"reRegisterMinIntervalMinutes"`
	Label                        string `json:"label"`
	// WireGuard is zero with tunnel masque, Masque with tunnel wireguard.
	WireGuard WireGuard `json:"wireguard,omitzero"`
	Masque    Masque    `json:"masque,omitzero"`
}

func init() {
	provider.Register(Type, provider.Factory{New: newWARP, Identification: parseIdentification, Candidates: true})
}

var required = schema.StrOpt{Required: true, Trim: true, MinLen: 1}

func parseIdentification(n *schema.Node) any {
	n.Strict("userAgent", "clientVersion", "apiVersion", "model", "locale", "osVersion")
	s := func(key string) string {
		v, _ := n.String(key, required)
		return v
	}
	id := Identification{
		UserAgent: s("userAgent"), ClientVersion: s("clientVersion"), APIVersion: s("apiVersion"),
		Model: s("model"), Locale: s("locale"), OSVersion: s("osVersion"),
	}
	if id.APIVersion != "" && !apiVersionPattern.MatchString(id.APIVersion) {
		n.Addf("apiVersion", "must match [A-Za-z0-9._-]+")
	}
	return id
}

func newWARP(n *schema.Node, env provider.Env) provider.Provider {
	n.Strict(slices.Concat(provider.CommonKeys, []string{
		"tunnel", "keys", "deviceLostAfterChecks", "reRegisterMinIntervalMinutes", "label", "wireguard", "masque",
	})...)
	w := &WARP{id: env.ID, out: env.Output, cfg: transport.Config{API: env.API}, now: time.Now}
	w.ident, _ = env.Identification.(Identification)
	w.Tunnel = n.Enum("tunnel", []string{tunnelWireGuard, tunnelMasque}, "")
	if keys := n.Object("keys", true); keys != nil {
		keys.Strict("rotateOnStart")
		if !keys.Has("rotateOnStart") {
			keys.Addf("rotateOnStart", "required")
		}
		w.Keys.RotateOnStart = keys.Bool("rotateOnStart", false)
	}
	one, two := 1, 2
	w.DeviceLostAfterChecks = n.Int("deviceLostAfterChecks", schema.IntOpt{Default: &two, Min: &one})
	w.ReRegisterMinIntervalMinutes = n.Int("reRegisterMinIntervalMinutes", schema.IntOpt{Required: true, Min: &one})
	w.Label, _ = n.String("label", required)
	switch w.Tunnel {
	case tunnelWireGuard:
		if n.Has("masque") {
			n.Addf("masque", "not allowed with tunnel wireguard")
		}
		w.parseWireGuard(n)
	case tunnelMasque:
		if n.Has("wireguard") {
			n.Addf("wireguard", "not allowed with tunnel masque")
		}
		w.parseMasque(n)
	}
	return w
}

func (w *WARP) parseWireGuard(n *schema.Node) {
	wg := n.Object("wireguard", false)
	wg.Strict("dns", "mtu", "allowedIPs", "port")
	w.WireGuard.DNS = cloudflareDNS
	if wg.Has("dns") {
		w.WireGuard.DNS = wg.StringList("dns", required)
	}
	minMTU, maxMTU, defMTU := 576, 1500, 1280
	w.WireGuard.MTU = wg.Int("mtu", schema.IntOpt{Default: &defMTU, Min: &minMTU, Max: &maxMTU})
	w.WireGuard.AllowedIPs = []string{"0.0.0.0/0", "::/0"}
	if wg.Has("allowedIPs") {
		w.WireGuard.AllowedIPs = wg.StringList("allowedIPs", required)
	}
	if wg.Has("port") {
		if w.WireGuard.Port = wg.Int("port", schema.IntOpt{}); !slices.Contains(wgPorts, w.WireGuard.Port) {
			wg.Addf("port", "must be one of 2408|500|1701|4500")
		}
	}
	// The config values go through the candidate parser, as the API values do.
	if _, err := w.line(profile{
		ClientID: "AAAA", V4: "192.0.2.2", PeerKey: dummyKey, Endpoint: "192.0.2.1:2408",
	}, [32]byte{}); err != nil {
		n.Addf("wireguard", "%v", err)
	}
}

func (w *WARP) parseMasque(n *schema.Node) {
	mq := n.Object("masque", true)
	if mq == nil {
		return
	}
	mq.Strict("transports", "sni", "port", "dns")
	for _, e := range mq.StringListElems("transports", required) {
		switch {
		case e.Value != candidate.TransportH3 && e.Value != candidate.TransportH2:
			mq.Addf(e.Key, "must be h3 or h2")
		case slices.Contains(w.Masque.Transports, e.Value):
			mq.Addf(e.Key, "duplicate transport %q", e.Value)
		default:
			w.Masque.Transports = append(w.Masque.Transports, e.Value)
		}
	}
	w.Masque.SNI, _ = mq.String("sni", required)
	minPort, maxPort, defPort := 1, 65535, 443
	w.Masque.Port = mq.Int("port", schema.IntOpt{Default: &defPort, Min: &minPort, Max: &maxPort})
	w.Masque.DNS = cloudflareDNS
	if mq.Has("dns") {
		w.Masque.DNS = mq.StringList("dns", required)
	}
	if len(w.Masque.Transports) == 0 || w.Masque.SNI == "" {
		return
	}
	// The config values go through the candidate parser with a dummy API
	// answer; an IP sni is checked against its own endpoint here, against the
	// API endpoint on every tick.
	endpoint := "192.0.2.1"
	if a, err := netip.ParseAddr(w.Masque.SNI); err == nil && a.Is4() {
		endpoint = a.String()
	}
	if _, err := w.masqueLines(profile{V4: "192.0.2.2", PeerKey: dummyPEM, EndpointV4: endpoint + ":0"}, dummyECKey); err != nil {
		n.Addf("masque", "%v", err)
	}
}

// dummyKey is a valid key for checking the config alone.
const dummyKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// dummyECKey and dummyPEM are valid P-256 keys for checking the config alone.
var (
	dummyECKey = func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err) // unreachable: crypto/rand does not fail
		}
		return k
	}()
	dummyPEM = func() string {
		b, err := x509.MarshalPKIXPublicKey(&dummyECKey.PublicKey)
		if err != nil {
			panic(err) // unreachable: a P-256 key always marshals
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
	}()
)

// ID implements provider.Provider.
func (w *WARP) ID() string { return w.id }

// Kind implements provider.Provider.
func (w *WARP) Kind() candidate.Kind {
	if w.Tunnel == tunnelMasque {
		return candidate.KindMasque
	}
	return candidate.KindWireGuard
}

// record is the state record: one device per output candidate, by slot.
type record struct {
	Devices []device `json:"devices"`
}

// device is one registered device; ID is "" for a slot without one.
type device struct {
	ID string `json:"id,omitempty"`
	// Token is the device Bearer token (secret).
	Token string `json:"token,omitempty"`
	// PrivateKey is the X25519 private key of the registration (secret).
	PrivateKey []byte `json:"privateKey,omitempty"`
	// MasqueKey is the PKCS#8 DER P-256 private key of tunnel masque
	// (secret); nil until a PATCH to MASQUE succeeded.
	MasqueKey []byte  `json:"masqueKey,omitempty"`
	Profile   profile `json:"profile,omitzero"`
	// LostChecks counts device-lost answers in a row.
	LostChecks int `json:"lostChecks,omitempty"`
	// RegisterAttemptAt is the last POST /reg attempt of the slot, saved
	// before the request; it stays when the device is lost and limits the
	// next attempt. Zero for a slot never registered.
	RegisterAttemptAt time.Time `json:"registerAttemptAt,omitzero"`
}

// profile are the API values of the tunnel, as the API sends them.
type profile struct {
	ClientID string `json:"clientId"`
	V4       string `json:"v4,omitempty"`
	V6       string `json:"v6,omitempty"`
	// PeerKey is base64 X25519 for WireGuard, a PEM PKIX P-256 key for MASQUE.
	PeerKey  string `json:"peerKey"`
	Endpoint string `json:"endpoint"`
	// EndpointV4 is endpoint.v4, "IP:0"; MASQUE uses it.
	EndpointV4 string `json:"endpointV4,omitempty"`
}

// regResponse is the part of a POST, GET or PATCH /reg answer wgp reads.
type regResponse struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Key     string `json:"key"`
	Enabled *bool  `json:"enabled"`
	Config  struct {
		ClientID  string `json:"client_id"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				Host string `json:"host"`
				V4   string `json:"v4"`
			} `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}

func (r regResponse) profile() profile {
	p := profile{ClientID: r.Config.ClientID, V4: r.Config.Interface.Addresses.V4, V6: r.Config.Interface.Addresses.V6}
	if len(r.Config.Peers) > 0 {
		e := r.Config.Peers[0].Endpoint
		p.PeerKey, p.Endpoint, p.EndpointV4 = r.Config.Peers[0].PublicKey, e.Host, e.V4
	}
	return p
}

// errLost marks a check answer that counts towards deviceLostAfterChecks.
var errLost = errors.New("device lost")

// Tick implements provider.Provider: every slot is checked (GET /reg/{id}),
// rotated once per process with keys.rotateOnStart (PATCH /reg/{id}) and
// registered (POST /reg) when it has no device. Every change is saved at
// once. A slot that fails is reported in the status; the other lines are
// still published; no line at all is an error.
func (w *WARP) Tick(ctx context.Context, s provider.Store) (provider.Result, error) {
	if w.api == nil {
		a, err := newAPI(w.ident, w.cfg)
		if err != nil {
			return provider.Result{}, err
		}
		w.api = a
	}
	var rec record
	if b, err := s.Load(); err != nil {
		return provider.Result{}, err
	} else if b != nil {
		if err := json.Unmarshal(b, &rec); err != nil {
			return provider.Result{}, errors.New("state record: invalid JSON")
		}
	}
	n := w.out.Candidates
	if len(rec.Devices) > n {
		rec.Devices = rec.Devices[:n] // shortcut: devices of dropped slots stay registered with Cloudflare unused
	}
	rec.Devices = append(rec.Devices, make([]device, n-len(rec.Devices))...)
	if w.rotate == nil {
		w.rotate = map[int]bool{}
		for i := range n {
			w.rotate[i] = w.Keys.RotateOnStart
		}
	}
	save := func() error {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return s.Save(b)
	}

	var res provider.Result
	var errs []string
	for i := range rec.Devices {
		lines, err := w.slot(ctx, i, &rec.Devices[i], save, &res.Events)
		if err != nil {
			if ctx.Err() != nil {
				return provider.Result{Events: res.Events}, ctx.Err()
			}
			errs = append(errs, fmt.Sprintf("device %d: %v", i+1, err))
		}
		res.Lines = append(res.Lines, lines...)
	}
	if len(res.Lines) == 0 && slices.ContainsFunc(rec.Devices, func(d device) bool { return d.ID != "" }) {
		return provider.Result{Events: res.Events}, errors.New(strings.Join(errs, "; ")) // a device that cannot make a line: keep the file
	}
	// No device left (lost, or not registered yet): no lines and no error,
	// so the output empties.
	res.Status = wgpstatus.Provider{State: wgpstatus.StateOK, Issued: len(res.Lines)}
	if len(errs) > 0 {
		res.Status.State, res.Status.Error = wgpstatus.StateError, strings.Join(errs, "; ")
	}
	return res, nil
}

// slot runs one device slot and returns its lines (none when it has no
// device); its decisions are appended to events. An error with lines means
// the lines are still published.
func (w *WARP) slot(ctx context.Context, i int, d *device, save func() error, events *[]provider.Event) (lines []string, err error) {
	event := func(level slog.Level, name, msg string, attrs ...slog.Attr) {
		*events = append(*events, provider.Event{Name: name, Msg: msg, Level: level, Slot: i + 1, Attrs: attrs})
	}
	var errs []error
	checked, registered := false, false
	if d.ID != "" {
		err := w.check(ctx, d)
		switch {
		case err == nil:
			d.LostChecks, checked = 0, true
		case errors.Is(err, errLost):
			d.LostChecks++
			// The counters go into the event, so the error text stays the
			// same from check to check.
			errs = append(errs, fmt.Errorf("check: %w", err))
			status, code := http.StatusOK, 0 // an answer with enabled false
			if se, ok := errors.AsType[*transport.StatusError](err); ok {
				status, code = se.Status, se.Code
			}
			event(slog.LevelWarn, "device_check_lost", "Device check counted as lost.",
				slog.Int("checks", d.LostChecks), slog.Int("after", w.DeviceLostAfterChecks), slog.Int("status", status), slog.Int("code", code))
			if d.LostChecks >= w.DeviceLostAfterChecks {
				event(slog.LevelWarn, "device_lost", "Device lost; its candidate is withdrawn.", slog.Int("checks", d.LostChecks))
				*d = device{RegisterAttemptAt: d.RegisterAttemptAt}
			}
		default: // 5xx, timeouts and the like neither count nor reset
			errs = append(errs, fmt.Errorf("check: %w", err))
		}
		if err := save(); err != nil {
			return nil, err
		}
	}
	if d.ID == "" {
		now := w.now()
		interval := time.Duration(w.ReRegisterMinIntervalMinutes) * time.Minute
		if next := d.RegisterAttemptAt.Add(interval); !d.RegisterAttemptAt.IsZero() && now.Before(next) {
			if msg := w.regErr[i]; msg != "" {
				return nil, join(append(errs, errors.New(msg)))
			}
			return nil, join(append(errs, fmt.Errorf("re-registration not before %s", next.UTC().Format(time.RFC3339))))
		}
		// The attempt counts even when it fails: no series of POST /reg.
		d.RegisterAttemptAt = now
		if err := save(); err != nil {
			return nil, err
		}
		if err := w.register(ctx, d, now); err != nil {
			next := now.Add(interval).UTC()
			if ctx.Err() == nil {
				event(slog.LevelWarn, "device_register_failed", "Device registration failed.",
					slog.String("error", err.Error()), slog.Time("notBefore", next))
			}
			msg := fmt.Sprintf("register: %v (next attempt not before %s)", err, next.Format(time.RFC3339))
			if w.regErr == nil {
				w.regErr = map[int]string{}
			}
			w.regErr[i] = msg
			return nil, join(append(errs, errors.New(msg)))
		}
		delete(w.regErr, i)
		event(slog.LevelInfo, "device_registered", "Device registered.")
		delete(w.rotate, i) // a new device has a new key
		if err := save(); err != nil {
			return nil, err
		}
		checked, registered = true, true
	}
	// A MASQUE device gets its P-256 key right after the registration, and
	// again until a PATCH succeeded.
	if checked && (w.rotate[i] || w.Tunnel == tunnelMasque && d.MasqueKey == nil) {
		reason := "retry"
		switch {
		case w.rotate[i]:
			reason = "rotate_on_start"
		case registered:
			reason = "new_device"
		}
		if err := w.rotateKey(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("rotate: %w", err))
		} else {
			keyType := "curve25519"
			if w.Tunnel == tunnelMasque {
				keyType = "secp256r1"
			}
			event(slog.LevelInfo, "device_key_rotated", "Device key rotated.", slog.String("keyType", keyType), slog.String("reasonCode", reason))
			delete(w.rotate, i)
			if err := save(); err != nil {
				return nil, err
			}
		}
	}
	lines, err = w.lines(*d)
	return lines, join(append(errs, err))
}

// lines builds the candidate lines of a device from the state record.
func (w *WARP) lines(d device) ([]string, error) {
	if w.Tunnel == tunnelMasque {
		if d.MasqueKey == nil {
			return nil, errors.New("no MASQUE key yet")
		}
		k, err := x509.ParsePKCS8PrivateKey(d.MasqueKey)
		key, ok := k.(*ecdsa.PrivateKey)
		if err != nil || !ok {
			return nil, errors.New("state record: bad MASQUE key")
		}
		return w.masqueLines(d.Profile, key)
	}
	if len(d.PrivateKey) != 32 {
		return nil, errors.New("state record: bad private key")
	}
	line, err := w.line(d.Profile, [32]byte(d.PrivateKey))
	if err != nil {
		return nil, err
	}
	return []string{line}, nil
}

// join joins the non-nil errors with "; ", nil when there is none.
func join(errs []error) error {
	var msgs []string
	for _, e := range errs {
		if e != nil {
			msgs = append(msgs, e.Error())
		}
	}
	if msgs == nil {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

// check asks GET /reg/{id}. 404/1006, 401/1012 and enabled=false wrap
// errLost. An answer for the current key refreshes the profile; right after
// a key change the API may still show the old key for a while.
func (w *WARP) check(ctx context.Context, d *device) error {
	r, err := w.call(ctx, http.MethodGet, "/reg/"+d.ID, d.Token, nil)
	if se, ok := errors.AsType[*transport.StatusError](err); ok &&
		(se.Status == http.StatusNotFound && se.Code == codeNotFound || se.Status == http.StatusUnauthorized && se.Code == codeNotAuthorized) {
		return fmt.Errorf("%w: %w", errLost, err)
	}
	switch {
	case err != nil:
		return err
	case r.Enabled == nil:
		return errors.New("answer without enabled")
	case !*r.Enabled:
		return fmt.Errorf("%w: enabled is false", errLost)
	}
	if r.Key == w.deviceKey(*d) {
		d.Profile = r.profile()
	}
	return nil
}

// register creates a device with a new X25519 key (POST /reg).
func (w *WARP) register(ctx context.Context, d *device, now time.Time) error {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	r, err := w.call(ctx, http.MethodPost, "/reg", "", regBody(w.ident, base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), now))
	if err != nil {
		return err
	}
	if !idPattern.MatchString(r.ID) || r.Token == "" {
		return errors.New("answer without a valid id or token")
	}
	*d = device{ID: r.ID, Token: r.Token, PrivateKey: key.Bytes(), Profile: r.profile(), RegisterAttemptAt: now}
	return nil
}

// rotateKey sets a new X25519 key (PATCH /reg/{id}); reserved changes with
// it, so the profile comes from the answer. The old key keeps working for a
// few minutes. With tunnel masque it sets a new P-256 key instead.
func (w *WARP) rotateKey(ctx context.Context, d *device) error {
	if w.Tunnel == tunnelMasque {
		return w.masqueKey(ctx, d)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	r, err := w.call(ctx, http.MethodPatch, "/reg/"+d.ID, d.Token,
		keyBody(base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), "curve25519", "wireguard"))
	if err != nil {
		return err
	}
	d.PrivateKey, d.Profile = key.Bytes(), r.profile()
	return nil
}

// masqueKey switches the device to MASQUE with a new P-256 key (PATCH
// /reg/{id}, secp256r1); the answer gives the endpoint key and address. A
// replaced MASQUE key stops working at once.
func (w *WARP) masqueKey(ctx context.Context, d *device) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	r, err := w.call(ctx, http.MethodPatch, "/reg/"+d.ID, d.Token,
		keyBody(base64.StdEncoding.EncodeToString(pub), "secp256r1", tunnelMasque))
	if err != nil {
		return err
	}
	d.MasqueKey, d.Profile = der, r.profile()
	return nil
}

// deviceKey is the device public key as the API shows it: base64 PKIX of
// the MASQUE key once set, else base64 X25519; "" if the key is invalid.
func (w *WARP) deviceKey(d device) string {
	if w.Tunnel != tunnelMasque || d.MasqueKey == nil {
		return publicKey(d.PrivateKey)
	}
	k, err := x509.ParsePKCS8PrivateKey(d.MasqueKey)
	key, ok := k.(*ecdsa.PrivateKey)
	if err != nil || !ok {
		return ""
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// call sends one request and decodes the answer. A *transport.StatusError
// gets the WARP code of errors[0]; no error holds the body.
func (w *WARP) call(ctx context.Context, method, path, token string, body map[string]string) (regResponse, error) {
	resp, err := w.api.do(ctx, method, path, token, body)
	if se, ok := errors.AsType[*transport.StatusError](err); ok {
		var v struct {
			Errors []struct {
				Code int `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(resp.Body, &v) == nil && len(v.Errors) > 0 {
			se.Code = v.Errors[0].Code
		}
		return regResponse{}, se
	}
	if err != nil {
		return regResponse{}, err
	}
	var r regResponse
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		return regResponse{}, errors.New("answer is not the expected JSON")
	}
	return r, nil
}

// publicKey is the base64 X25519 public key of priv, "" if priv is invalid.
func publicKey(priv []byte) string {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

// line builds the candidate line of a device. Errors name the field, never
// the value.
func (w *WARP) line(p profile, key [32]byte) (string, error) {
	c := candidate.WireGuard{Provider: Type, PrivateKey: key, MTU: w.WireGuard.MTU, Label: w.Label}
	reserved, err := base64.StdEncoding.DecodeString(p.ClientID)
	if err != nil || len(reserved) != 3 {
		return "", errors.New("client_id: reserved must be exactly 3 bytes")
	}
	c.Reserved = (*[3]byte)(reserved)
	peer, err := base64.StdEncoding.DecodeString(p.PeerKey)
	if err != nil || len(peer) != 32 {
		return "", errors.New("peer public_key: must be a 32-byte base64 key")
	}
	c.PublicKey = [32]byte(peer)
	c.Endpoint = p.Endpoint
	if w.WireGuard.Port != 0 {
		host, _, err := net.SplitHostPort(p.Endpoint)
		if err != nil {
			return "", errors.New("peer endpoint: must be host:port")
		}
		c.Endpoint = net.JoinHostPort(host, strconv.Itoa(w.WireGuard.Port))
	}
	if c.Address, err = addresses(p); err != nil {
		return "", err
	}
	if c.DNS, err = dnsIPs(w.WireGuard.DNS); err != nil {
		return "", err
	}
	for _, s := range w.WireGuard.AllowedIPs {
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return "", errors.New("allowedIPs: each must be a CIDR prefix")
		}
		c.AllowedIPs = append(c.AllowedIPs, pfx)
	}
	line := candidate.FormatWireGuard(c)
	if _, err := candidate.ParseWireGuard(line); err != nil {
		return "", err
	}
	return line, nil
}

// addresses are the device addresses of p as full-length prefixes.
func addresses(p profile) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, a := range []string{p.V4, p.V6} {
		if a == "" {
			continue
		}
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return nil, errors.New("interface addresses: must be IP addresses")
		}
		out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return out, nil
}

// dnsIPs parses the configured DNS servers.
func dnsIPs(list []string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range list {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, errors.New("dns: each must be an IP address")
		}
		out = append(out, ip)
	}
	return out, nil
}

// masqueLines builds one candidate line of a MASQUE device per transport,
// with the transport index as priority: endpoint.v4 with masque.port, the
// PEM endpoint key and the device addresses of the API. Errors name the
// field, never the value.
func (w *WARP) masqueLines(p profile, key *ecdsa.PrivateKey) ([]string, error) {
	c := candidate.Masque{Provider: Type, PrivateKey: key, SNI: w.Masque.SNI, Label: w.Label}
	blk, _ := pem.Decode([]byte(p.PeerKey))
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return nil, errors.New("peer public_key: must be a PEM PKIX ECDSA P-256 key")
	}
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if peer, ok := pk.(*ecdsa.PublicKey); err == nil && ok && peer.Curve == elliptic.P256() {
		c.PeerKey = peer
	} else {
		return nil, errors.New("peer public_key: must be a PEM PKIX ECDSA P-256 key")
	}
	host, _, err := net.SplitHostPort(p.EndpointV4)
	ip, perr := netip.ParseAddr(host)
	if err != nil || perr != nil || !ip.Is4() {
		return nil, errors.New("peer endpoint v4: must be IPv4:port")
	}
	c.Endpoint = netip.AddrPortFrom(ip, uint16(w.Masque.Port)) // masque.port is 1..65535
	if c.Address, err = addresses(p); err != nil {
		return nil, err
	}
	if c.DNS, err = dnsIPs(w.Masque.DNS); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(w.Masque.Transports))
	for i, t := range w.Masque.Transports {
		c.Transport, c.Priority = t, i
		line := candidate.FormatMasque(c)
		if _, err := candidate.ParseMasque(line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}
