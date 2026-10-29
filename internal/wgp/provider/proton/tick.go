package proton

import (
	"cmp"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
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
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// keysKey is the record member of the candidate keys.
const keysKey = "keys"

// keyState is the keys member of the state record.
type keyState struct {
	// Gen is the current key generation, from 1; every rotation adds one.
	Gen int `json:"gen"`
	// RotatedAt is when the last slot moved to Gen. The certificates of
	// older generations are revoked revokeOldAfterMinutes later; zero when
	// none waits.
	RotatedAt time.Time `json:"rotatedAt,omitzero"`
	// Slots are the candidates of the output, by number - 1.
	Slots []slot `json:"slots"`
}

// slot is one candidate: its key, its persistent certificate and its
// server. Seed is nil until the first certificate was issued.
type slot struct {
	// Seed is the Ed25519 seed of the key (secret).
	Seed      []byte    `json:"seed,omitempty"`
	Gen       int       `json:"gen,omitempty"`
	Serial    string    `json:"serial,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	RefreshAt time.Time `json:"refreshAt,omitzero"`
	// Server is the ID of the logical server, "" without one.
	Server string `json:"server,omitempty"`
}

// logical is a logical server of /vpn/v1/logicals.
type logical struct {
	ID, Name, City                         string
	EntryCountry, ExitCountry, HostCountry string
	Tier, Features, Status, Load           int
	Score                                  float64
	Servers                                []physical
	// pick is the physical server of the candidate.
	pick physical
}

// physical is a physical server of a logical one.
type physical struct {
	ID, EntryIP, X25519PublicKey string
	Status                       int
}

// entry is the country of the entry point: EntryCountry for Secure Core,
// HostCountry for Smart Routing, else ExitCountry.
func (l *logical) entry() string {
	switch {
	case l.Features&featSecureCore != 0:
		return l.EntryCountry
	case l.HostCountry != "":
		return l.HostCountry
	}
	return l.ExitCountry
}

// Tick implements provider.Provider: the account and the server list, then
// the persistent certificate of every candidate slot (issue, rotation at
// start, renewal), the server of every slot, the revocation of the older
// generations, and one line per slot with a certificate and a server. Every
// state change is saved at once. Without a valid session the tick fails
// with state needs_login and changes nothing.
func (p *Proton) Tick(ctx context.Context, s provider.Store) (provider.Result, error) {
	p.takeEvents() // left by login or logout
	var events []provider.Event
	// add appends events after the session events so far, in order.
	add := func(e ...provider.Event) { events = append(append(events, p.takeEvents()...), e...) }
	ks, err := p.loadKeys(s)
	if err != nil {
		return provider.Result{}, err
	}
	fail := func(err error) (provider.Result, error) {
		add()
		res := provider.Result{Status: p.status(ks), Events: events}
		if errors.Is(err, errNeedsLogin) {
			// The same text on every tick, whichever call found the dead
			// session, so the failure is logged once.
			res.Status.State, err = wgpstatus.StateNeedsLogin, errNeedsLogin
		}
		return res, err
	}
	maxTier, maxConnect, err := p.account(ctx, s)
	if err != nil {
		return fail(err)
	}
	if lv := tierLevels[p.Tier]; lv[0] > maxTier {
		return fail(fmt.Errorf("proton: tier %s needs MaxTier %d, the account has MaxTier %d", p.Tier, lv[0], maxTier))
	}
	set, all, rejected, err := p.servers(ctx, s, maxTier)
	if err != nil {
		return fail(err)
	}
	save := func() error { return saveMember(s, keysKey, ks) }

	n := p.out.Candidates
	if len(ks.Slots) > n {
		ks.Slots = ks.Slots[:n] // shortcut: certificates of dropped slots of the current generation stay until the next rotation revokes them
	}
	ks.Slots = append(ks.Slots, make([]slot, n-len(ks.Slots))...)
	if ks.Gen == 0 {
		ks.Gen = 1
	}
	if p.rotate {
		if slices.ContainsFunc(ks.Slots, func(sl slot) bool { return sl.Seed != nil }) {
			ks.Gen++
			if err := save(); err != nil {
				return fail(err)
			}
			add(provider.Event{Name: "keys_rotated", Msg: "Key generation advanced.", Level: slog.LevelInfo, Attrs: []slog.Attr{slog.Int("gen", ks.Gen)}})
		}
		p.rotate = false
	}

	var errs []string
	now := p.now()
	for i := range ks.Slots {
		changed, ev, err := p.certify(ctx, s, i, &ks, now)
		if changed {
			add(ev)
		}
		if err != nil {
			if errors.Is(err, errNeedsLogin) || ctx.Err() != nil {
				return fail(err)
			}
			errs = append(errs, fmt.Sprintf("candidate %d: %v", i+1, err))
		}
		if changed {
			if err := save(); err != nil {
				return fail(err)
			}
		}
	}
	if ev := p.assign(ks.Slots, set, all, rejected); ev != nil {
		add(ev...)
		if err := save(); err != nil {
			return fail(err)
		}
	}
	if !ks.RotatedAt.IsZero() && !now.Before(ks.RotatedAt.Add(p.revokeAfter)) {
		held := map[string]bool{}
		for _, sl := range ks.Slots {
			held[sl.Serial] = true
		}
		revoked, already, err := p.revoke(ctx, s, func(gen int, serial string) bool { return gen != ks.Gen && !held[serial] })
		if revoked > 0 || already > 0 {
			level := slog.LevelInfo
			if revoked == 0 {
				level = slog.LevelDebug // only certificates an earlier attempt revoked
			}
			add(provider.Event{Name: "certificates_revoked", Msg: "Older certificates revoked.", Level: level, Attrs: []slog.Attr{
				slog.Int("count", revoked), slog.Int("alreadyRevoked", already), slog.Int("gen", ks.Gen),
			}})
		}
		if err != nil {
			if errors.Is(err, errNeedsLogin) || ctx.Err() != nil {
				return fail(err)
			}
			errs = append(errs, fmt.Sprintf("revoke the older generations: %v", err))
		} else if !slices.ContainsFunc(ks.Slots, func(sl slot) bool { return sl.Seed != nil && sl.Gen != ks.Gen }) {
			ks.RotatedAt = time.Time{}
			if err := save(); err != nil {
				return fail(err)
			}
		}
	}

	byID := map[string]*logical{}
	for i := range set {
		byID[set[i].ID] = &set[i]
	}
	var res provider.Result
	for i, sl := range ks.Slots {
		l := byID[sl.Server]
		switch {
		case sl.Seed == nil || !now.Before(sl.ExpiresAt):
			continue // its error is in errs
		case l == nil:
			errs = append(errs, fmt.Sprintf("candidate %d: no free server matches the filters", i+1))
			continue
		}
		line, err := p.line(sl, l)
		if err != nil {
			errs = append(errs, fmt.Sprintf("candidate %d: %v", i+1, err))
			continue
		}
		res.Lines = append(res.Lines, line)
	}
	res.Status = p.status(ks)
	res.Status.Limit = maxConnect
	if len(res.Lines) == 0 {
		// No line: the previous file stays.
		return fail(errors.New(strings.Join(append(errs, "no candidate"), "; ")))
	}
	if len(errs) > 0 {
		res.Status.State, res.Status.Error = wgpstatus.StateError, strings.Join(errs, "; ")
	}
	add()
	res.Events = events
	return res, nil
}

// status is the issued count and the earliest expiry of the slots with a
// certificate that has not expired.
func (p *Proton) status(ks keyState) wgpstatus.Provider {
	var st wgpstatus.Provider
	now := p.now()
	for _, sl := range ks.Slots {
		if sl.Seed == nil || !now.Before(sl.ExpiresAt) {
			continue
		}
		st.Issued++
		if st.ExpiresAt.IsZero() || sl.ExpiresAt.Before(st.ExpiresAt) {
			st.ExpiresAt = sl.ExpiresAt.UTC()
		}
	}
	return st
}

// loadKeys returns the keys member of the state record.
func (p *Proton) loadKeys(s provider.Store) (keyState, error) {
	rec, err := loadRecord(s)
	if err != nil {
		return keyState{}, err
	}
	var ks keyState
	if raw, ok := rec[keysKey]; ok && json.Unmarshal(raw, &ks) != nil {
		return keyState{}, errors.New("proton: state record: bad keys")
	}
	for _, sl := range ks.Slots {
		if sl.Seed != nil && len(sl.Seed) != ed25519.SeedSize {
			return keyState{}, errors.New("proton: state record: bad key seed")
		}
	}
	return ks, nil
}

// account returns MaxTier and MaxConnect of GET /vpn/v2. The answer also
// holds the OpenVPN credentials: they are not read.
func (p *Proton) account(ctx context.Context, s provider.Store) (maxTier, maxConnect int, err error) {
	resp, err := p.authed(ctx, s, call{method: http.MethodGet, path: "/vpn/v2"})
	if err != nil {
		return 0, 0, fmt.Errorf("proton: account: %w", err)
	}
	var v struct {
		VPN struct {
			MaxTier, MaxConnect *int
		}
	}
	if json.Unmarshal(resp.Body, &v) != nil || v.VPN.MaxTier == nil || v.VPN.MaxConnect == nil {
		return 0, 0, errors.New("proton: account: no MaxTier or MaxConnect in the answer")
	}
	return *v.VPN.MaxTier, *v.VPN.MaxConnect, nil
}

// logicalsCall is the server list request of a tick: the headers of the
// client without X-PM-netzone and If-Modified-Since.
func logicalsCall() call {
	return call{method: http.MethodGet, path: "/vpn/v1/logicals?SecureCoreFilter=all&WithState=true"}
}

// servers returns the logical servers that pass the filters, best first
// (Score, then Load, then Name), each with its physical server: the online
// one with the smallest ID. It also returns every logical server of the
// answer and, for those that do not pass, why (see reject).
func (p *Proton) servers(ctx context.Context, s provider.Store, maxTier int) (set []logical, all map[string]logical, rejected map[string]string, err error) {
	resp, err := p.authed(ctx, s, logicalsCall())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("proton: server list: %w", err)
	}
	var v struct{ LogicalServers []logical }
	if json.Unmarshal(resp.Body, &v) != nil {
		return nil, nil, nil, errors.New("proton: server list: bad answer")
	}
	all, rejected = map[string]logical{}, map[string]string{}
	for _, l := range v.LogicalServers {
		all[l.ID] = l
		if why := p.reject(&l, maxTier); why != "" {
			rejected[l.ID] = why
			continue
		}
		set = append(set, l)
	}
	slices.SortStableFunc(set, func(a, b logical) int {
		return cmp.Or(cmp.Compare(a.Score, b.Score), cmp.Compare(a.Load, b.Load), strings.Compare(a.Name, b.Name))
	})
	return set, all, rejected, nil
}

// reject returns why logical server l does not pass the filters (offline,
// tier, filter, load, score or no_physical), "" when it does; then l.pick
// is its physical server.
func (p *Proton) reject(l *logical, maxTier int) string {
	lv := tierLevels[p.Tier]
	var require, exclude int
	for _, f := range p.Servers.RequireFeatures {
		require |= featureBits[f]
	}
	for _, f := range p.Servers.ExcludeFeatures {
		exclude |= featureBits[f]
	}
	in := func(list []string, v string) bool { return len(list) == 0 || slices.Contains(list, v) }
	switch {
	case l.Status != 1:
		return "offline"
	case l.Tier < lv[0], l.Tier > min(lv[1], maxTier):
		return "tier"
	case !in(p.Servers.EntryCountries, l.entry()), !in(p.Servers.ExitCountries, l.ExitCountry),
		!in(p.Servers.Cities, l.City),
		l.Features&require != require, l.Features&exclude != 0:
		return "filter"
	case l.Load > p.Servers.MaxLoad:
		return "load"
	case l.Score > float64(p.Servers.MaxScore), l.Score >= 1000:
		return "score"
	}
	ok := false
	for _, ph := range l.Servers {
		if ph.Status == 1 && validPhysical(ph) && (!ok || ph.ID < l.pick.ID) {
			l.pick, ok = ph, true
		}
	}
	switch {
	case !ok:
		return "no_physical"
	case !countryPattern.MatchString(l.ExitCountry) || !countryPattern.MatchString(l.entry()):
		return "filter"
	}
	return ""
}

// validPhysical reports whether a physical server can make a line: an IPv4
// entry and a 32-byte key.
func validPhysical(ph physical) bool {
	a, err := netip.ParseAddr(ph.EntryIP)
	k, kerr := base64.StdEncoding.DecodeString(ph.X25519PublicKey)
	return err == nil && a.Is4() && kerr == nil && len(k) == 32
}

// assign gives every slot a logical server of set, never one server to two
// slots. A slot whose server left the set gets the best free one at once.
// A slot keeps its server until a free one has a Score lower by at least
// servers.switchMarginPercent on servers.switchConfirmChecks ticks in a row.
// It returns a candidate_server_changed event per slot whose server
// changed (nil when none did); all and rejected are the servers of the
// answer and why they are out of set (see servers).
func (p *Proton) assign(slots []slot, set []logical, all map[string]logical, rejected map[string]string) []provider.Event {
	before := make([]string, len(slots))
	reason := make([]string, len(slots))
	for i := range slots {
		before[i] = slots[i].Server
	}
	pos := map[string]int{}
	for i, l := range set {
		pos[l.ID] = i
	}
	used := map[string]bool{}
	for i := range slots {
		if _, ok := pos[slots[i].Server]; ok && !used[slots[i].Server] {
			used[slots[i].Server] = true
		} else {
			if slots[i].Server != "" {
				reason[i] = "gone"
			}
			slots[i].Server = ""
			delete(p.better, i)
		}
	}
	free := func() *logical {
		for i := range set {
			if !used[set[i].ID] {
				return &set[i]
			}
		}
		return nil
	}
	for i := range slots {
		if slots[i].Server == "" {
			if b := free(); b != nil {
				slots[i].Server, used[b.ID] = b.ID, true
			}
		}
	}
	margin := float64(100-p.Servers.SwitchMarginPercent) / 100
	for i := range slots {
		cur, ok := pos[slots[i].Server]
		if !ok {
			continue
		}
		b := free()
		if b == nil || b.Score >= set[cur].Score || b.Score > set[cur].Score*margin {
			delete(p.better, i)
			continue
		}
		if p.better[i]++; p.better[i] >= p.Servers.SwitchConfirmChecks {
			used[slots[i].Server] = false
			slots[i].Server, used[b.ID] = b.ID, true
			reason[i] = "better"
			delete(p.better, i)
		}
	}
	var events []provider.Event
	for i := range slots {
		if before[i] == slots[i].Server {
			continue
		}
		from, fromKnown := all[before[i]] // "" for an empty slot and a server missing from the answer
		to, toKnown := all[slots[i].Server]
		code := cmp.Or(reason[i], "vacant")
		attrs := []slog.Attr{slog.String("fromServer", from.Name), slog.String("toServer", to.Name), slog.String("reasonCode", code)}
		if code == "gone" {
			detail := rejected[before[i]]
			switch {
			case !fromKnown:
				detail = "missing"
			case detail == "":
				detail = "duplicate" // two slots held one server (a hand-edited record)
			}
			attrs = append(attrs, slog.String("detail", detail))
		}
		if fromKnown {
			attrs = append(attrs, slog.Float64("fromScore", from.Score))
		}
		if toKnown {
			attrs = append(attrs, slog.Float64("toScore", to.Score))
		}
		events = append(events, provider.Event{Name: "candidate_server_changed", Msg: "Candidate server changed.", Level: slog.LevelInfo, Slot: i + 1, Attrs: attrs})
	}
	return events
}

// certify gives slot i a valid persistent certificate: a new key at the
// current generation when it has none or an older one (the rotation), else
// a renewal of the same key once renewBeforeMinutes before the expiry or at
// RefreshTime, whichever is first. A failed request keeps the slot as it
// was. It reports whether the slot changed, with its certificate_issued
// event.
func (p *Proton) certify(ctx context.Context, s provider.Store, i int, ks *keyState, now time.Time) (bool, provider.Event, error) {
	sl := &ks.Slots[i]
	seed, renew := sl.Seed, true
	if sl.Seed == nil || sl.Gen != ks.Gen {
		seed, renew = make([]byte, ed25519.SeedSize), false
		_, _ = rand.Read(seed) // crypto/rand never fails
	} else {
		renewAt := sl.ExpiresAt.Add(-time.Duration(p.Certificate.RenewBeforeMinutes) * time.Minute)
		if !sl.RefreshAt.IsZero() && sl.RefreshAt.Before(renewAt) {
			renewAt = sl.RefreshAt
		}
		if now.Before(renewAt) {
			return false, provider.Event{}, nil
		}
	}
	next, err := p.issue(ctx, s, seed, ks.Gen, i, renew)
	if err != nil {
		if renew {
			return false, provider.Event{}, fmt.Errorf("renew: %w", err)
		}
		return false, provider.Event{}, fmt.Errorf("issue: %w", err)
	}
	mode := "new"
	switch {
	case renew:
		mode = "renew"
	case sl.Seed != nil:
		mode = "rotation"
		ks.RotatedAt = now
	}
	next.Server = sl.Server
	*sl = next
	return true, provider.Event{Name: "certificate_issued", Msg: "Certificate issued.", Level: slog.LevelInfo, Slot: i + 1, Attrs: []slog.Attr{
		slog.Int("gen", next.Gen), slog.String("mode", mode), slog.String("deviceName", p.deviceName(next.Gen, i)), slog.Time("expiresAt", next.ExpiresAt),
	}}, nil
}

// deviceName is the DeviceName of slot i at generation gen.
func (p *Proton) deviceName(gen, i int) string {
	return fmt.Sprintf("%s%s-%d-%d", p.prefix, p.out.ID, gen, i+1)
}

// namePattern matches exactly the DeviceName of this provider's output:
// <prefix><output id>-<gen>-<n>. Output ids have no "-", so another prefix
// or another output never matches.
func (p *Proton) namePattern() *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(p.prefix+p.out.ID) + "-([0-9]+)-([0-9]+)$")
}

// certificateCall is the persistent certificate request for key seed, in
// the body order ClientPublicKey, Duration, Features, Mode, DeviceName and,
// on a renewal of the same key, Renew.
func (p *Proton) certificateCall(seed []byte, name string, renew bool) (call, error) {
	der, err := x509.MarshalPKIXPublicKey(ed25519.NewKeyFromSeed(seed).Public())
	if err != nil {
		return call{}, err
	}
	f := p.Certificate.Features
	body := obj{
		{"ClientPublicKey", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))},
		{"Duration", strconv.Itoa(p.Certificate.DurationMinutes) + " min"},
		{"Features", obj{{"RandomNAT", !f.ModerateNAT}, {"SplitTCP", f.VPNAccelerator}, {"PortForwarding", false}, {"NetShieldLevel", f.NetShield}}},
		{"Mode", "persistent"},
		{"DeviceName", name},
	}
	if renew {
		body = append(body, kv{"Renew", true})
	}
	return call{method: http.MethodPost, path: "/vpn/v1/certificate", body: body}, nil
}

// issue requests the persistent certificate of slot i at generation gen
// for seed and checks that the answer is for its key.
func (p *Proton) issue(ctx context.Context, s provider.Store, seed []byte, gen, i int, renew bool) (slot, error) {
	c, err := p.certificateCall(seed, p.deviceName(gen, i), renew)
	if err != nil {
		return slot{}, err
	}
	resp, err := p.authed(ctx, s, c)
	if err != nil {
		return slot{}, err
	}
	var r struct {
		SerialNumber, ClientKeyFingerprint string
		ExpirationTime, RefreshTime        int64
	}
	if json.Unmarshal(resp.Body, &r) != nil || r.SerialNumber == "" || r.ExpirationTime <= 0 {
		return slot{}, errors.New("bad certificate answer")
	}
	_, pub := wgKey(seed)
	if fp := sha512.Sum512(pub[:]); r.ClientKeyFingerprint != base64.StdEncoding.EncodeToString(fp[:]) {
		return slot{}, errors.New("the certificate is not for the candidate key (ClientKeyFingerprint)")
	}
	sl := slot{Seed: seed, Gen: gen, Serial: r.SerialNumber, ExpiresAt: time.Unix(r.ExpirationTime, 0).UTC()}
	if r.RefreshTime > 0 {
		sl.RefreshAt = time.Unix(r.RefreshTime, 0).UTC()
	}
	return sl, nil
}

// wgKey is the WireGuard key of an Ed25519 seed: the clamped first half of
// SHA-512(seed), the X25519 scalar of the Ed25519 key, and its public key.
func wgKey(seed []byte) (sk, pub [32]byte) {
	h := sha512.Sum512(seed)
	copy(sk[:], h[:32])
	sk[0] &= 248
	sk[31] &= 127
	sk[31] |= 64
	k, err := ecdh.X25519().NewPrivateKey(sk[:])
	if err != nil {
		panic(err) // unreachable: the key is 32 bytes
	}
	return sk, [32]byte(k.PublicKey().Bytes())
}

// listPage is the size of a certificate list page.
const listPage = 50

// listCall lists one page of the persistent certificates: the first, or
// the one after serial begin.
func listCall(begin string) call {
	path := "/vpn/v1/certificate/all?Mode=persistent&Limit=" + strconv.Itoa(listPage)
	if begin != "" {
		path += "&BeginID=" + url.QueryEscape(begin)
	}
	return call{method: http.MethodGet, path: path}
}

// revokeCall revokes one certificate by its serial number.
func revokeCall(serial string) call {
	return call{method: http.MethodDelete, path: "/vpn/v1/certificate", body: obj{{"SerialNumber", serial}}}
}

// listed is a certificate of the list.
type listed struct{ SerialNumber, DeviceName string }

// list returns every persistent certificate, page by page: a full page
// means another one may follow after its last serial.
func (p *Proton) list(ctx context.Context, s provider.Store) ([]listed, error) {
	var all []listed
	begin := ""
	for {
		resp, err := p.authed(ctx, s, listCall(begin))
		if err != nil {
			return nil, fmt.Errorf("list certificates: %w", err)
		}
		var v struct {
			Code         int
			Certificates []listed
		}
		if json.Unmarshal(resp.Body, &v) != nil {
			return nil, errors.New("list certificates: bad answer")
		}
		if v.Code != 1000 && v.Code != 1001 {
			return nil, fmt.Errorf("list certificates: code %d", v.Code)
		}
		all = append(all, v.Certificates...)
		if len(v.Certificates) < listPage {
			return all, nil
		}
		next := v.Certificates[len(v.Certificates)-1].SerialNumber
		if next == "" || next == begin {
			return nil, errors.New("list certificates: the page has no next serial")
		}
		begin = next
	}
}

// revoke lists the certificates and revokes those whose DeviceName is
// exactly this provider's template and for which match(gen, serial) holds.
// Other names, session certificates (an empty DeviceName) included, are
// never touched. Count 0 (already revoked, maybe by an earlier attempt) is
// not an error: it is counted in already.
func (p *Proton) revoke(ctx context.Context, s provider.Store, match func(gen int, serial string) bool) (revoked, already int, err error) {
	certs, err := p.list(ctx, s)
	if err != nil {
		return 0, 0, err
	}
	re := p.namePattern()
	for _, c := range certs {
		m := re.FindStringSubmatch(c.DeviceName)
		if m == nil || c.SerialNumber == "" {
			continue
		}
		gen, err := strconv.Atoi(m[1])
		if err != nil || !match(gen, c.SerialNumber) {
			continue
		}
		resp, err := p.authed(ctx, s, revokeCall(c.SerialNumber))
		if err != nil {
			return revoked, already, fmt.Errorf("revoke %s: %w", c.DeviceName, err)
		}
		var r struct{ Count *int }
		if json.Unmarshal(resp.Body, &r) != nil || r.Count == nil {
			return revoked, already, fmt.Errorf("revoke %s: bad answer", c.DeviceName)
		}
		if *r.Count == 0 {
			already++
		} else {
			revoked++
		}
	}
	return revoked, already, nil
}

// line builds the candidate line of a slot on logical server l. Errors name
// the field, never the value.
func (p *Proton) line(sl slot, l *logical) (string, error) {
	sk, _ := wgKey(sl.Seed)
	peer, _ := base64.StdEncoding.DecodeString(l.pick.X25519PublicKey) // checked by validPhysical
	load, score := l.Load, l.Score
	c := candidate.WireGuard{
		Provider: Type, PrivateKey: sk, PublicKey: [32]byte(peer), MTU: p.WireGuard.MTU, Keepalive: p.WireGuard.Keepalive,
		Endpoint: net.JoinHostPort(l.pick.EntryIP, strconv.Itoa(p.WireGuard.Port)),
		Address:  []netip.Prefix{netip.MustParsePrefix("10.2.0.2/32")},
		Label:    label(p.Label, l),
		Meta:     candidate.Meta{Server: l.Name, Entry: l.entry(), Exit: l.ExitCountry, Load: &load, Score: &score, Expires: sl.ExpiresAt},
	}
	allowed := p.WireGuard.AllowedIPs
	if allowed == nil {
		allowed = []string{"0.0.0.0/0"}
	}
	if l.Features&featIPv6 != 0 {
		c.Address = append(c.Address, netip.MustParsePrefix("2a07:b944::2:2/128"))
		if p.WireGuard.AllowedIPs == nil {
			allowed = append(allowed, "::/0")
		}
	}
	for _, a := range allowed {
		c.AllowedIPs = append(c.AllowedIPs, netip.MustParsePrefix(a)) // checked by the config
	}
	for _, d := range p.WireGuard.DNS {
		c.DNS = append(c.DNS, netip.MustParseAddr(d)) // checked by the config
	}
	line := candidate.FormatWireGuard(c)
	if _, err := candidate.ParseWireGuard(line); err != nil {
		return "", err
	}
	return line, nil
}

// label fills the label template: {flag} of the exit country, {city} and
// {name}.
func label(tmpl string, l *logical) string {
	var flag strings.Builder
	for _, r := range l.ExitCountry {
		flag.WriteRune(0x1F1E6 + r - 'A') // regional indicator; ExitCountry is checked to be A-Z
	}
	return strings.NewReplacer("{flag}", flag.String(), "{city}", l.City, "{name}", l.Name).Replace(tmpl)
}
