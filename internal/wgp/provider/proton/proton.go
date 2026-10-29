package proton

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// Type is the provider type name.
const Type = "proton"

// Logical server feature bits.
const (
	featSecureCore = 1
	featIPv6       = 16
)

var (
	// tierLevels are the Tier values of the tier key: free takes Tier 0
	// only, plus Tier 1..2.
	tierLevels = map[string][2]int{"free": {0, 0}, "plus": {1, 2}}
	// featureBits are the names of the Features bits.
	featureBits = map[string]int{"secureCore": featSecureCore, "tor": 2, "p2p": 4, "streaming": 8, "ipv6": featIPv6}
	// wgPorts are the WireGuard ports of the Proton servers.
	wgPorts = []int{443, 88, 1224, 51820, 500, 4500}
	// countryPattern is an ISO2 country code as the API sends it.
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
)

// defaultLabel is the default label template.
const defaultLabel = "{flag} {city}, Proton {name}"

// settings are the config keys of a Proton provider.
type settings struct {
	Tier string `json:"tier"`
	Keys struct {
		RotateOnStart bool `json:"rotateOnStart"`
	} `json:"keys"`
	Certificate struct {
		DurationMinutes    int `json:"durationMinutes"`
		RenewBeforeMinutes int `json:"renewBeforeMinutes"`
		Features           struct {
			NetShield      int  `json:"netShield"`
			VPNAccelerator bool `json:"vpnAccelerator"`
			ModerateNAT    bool `json:"moderateNat"`
		} `json:"features"`
	} `json:"certificate"`
	Servers struct {
		ExitCountries       []string `json:"exitCountries"`
		EntryCountries      []string `json:"entryCountries"`
		Cities              []string `json:"cities"`
		RequireFeatures     []string `json:"requireFeatures"`
		ExcludeFeatures     []string `json:"excludeFeatures"`
		MaxLoad             int      `json:"maxLoad"`
		MaxScore            int      `json:"maxScore"`
		SwitchMarginPercent int      `json:"switchMarginPercent"`
		SwitchConfirmChecks int      `json:"switchConfirmChecks"`
	} `json:"servers"`
	WireGuard struct {
		Port int      `json:"port"`
		DNS  []string `json:"dns"`
		MTU  int      `json:"mtu"`
		// AllowedIPs is nil for the default: 0.0.0.0/0, and ::/0 on an
		// IPv6 server.
		AllowedIPs []string `json:"allowedIPs,omitempty"`
		Keepalive  int      `json:"keepalive,omitzero"`
	} `json:"wireguard"`
	Label string `json:"label"`
}

func init() {
	provider.Register(Type, provider.Factory{New: newFromConfig, Identification: parseIdentification, Candidates: true})
}

var required = schema.StrOpt{Required: true, Trim: true, MinLen: 1}

func parseIdentification(n *schema.Node) any {
	n.Strict("appVersion", "userAgent", "timezone")
	s := func(key string) string {
		v, _ := n.String(key, required)
		return v
	}
	return Identification{AppVersion: s("appVersion"), UserAgent: s("userAgent"), Timezone: provider.Timezone(n, "timezone")}
}

func newFromConfig(n *schema.Node, env provider.Env) provider.Provider {
	n.Strict(slices.Concat(provider.CommonKeys, []string{"tier", "keys", "certificate", "servers", "wireguard", "label"})...)
	ident, _ := env.Identification.(Identification)
	p, err := newProton(env.ID, ident, transport.Config{API: env.API})
	if err != nil {
		n.Addf("", "%v", err)
		return nil
	}
	p.out, p.prefix = env.Output, env.DeviceNamePrefix
	p.revokeAfter = time.Duration(env.RevokeOldAfterMinutes) * time.Minute
	if env.RevokeOldAfterMinutes == 0 && n.Bool("enabled", true) {
		n.Addf("", "keys.revokeOldAfterMinutes (top level) is required with an enabled proton provider")
	}
	s := &p.settings
	s.Tier = n.Enum("tier", []string{"free", "plus"}, "")
	if keys := n.Object("keys", true); keys != nil {
		keys.Strict("rotateOnStart")
		requireBool(keys, "rotateOnStart", &s.Keys.RotateOnStart)
	}
	p.rotate = s.Keys.RotateOnStart
	zero, one, two, ten, hundred := 0, 1, 2, 10, 100
	if c := n.Object("certificate", true); c != nil {
		c.Strict("durationMinutes", "renewBeforeMinutes", "features")
		maxDuration := 525600
		s.Certificate.DurationMinutes = c.Int("durationMinutes", schema.IntOpt{Required: true, Min: &ten, Max: &maxDuration})
		s.Certificate.RenewBeforeMinutes = c.Int("renewBeforeMinutes", schema.IntOpt{Required: true, Min: &one})
		if s.Certificate.DurationMinutes > 0 && s.Certificate.RenewBeforeMinutes >= s.Certificate.DurationMinutes {
			c.Addf("renewBeforeMinutes", "must be less than durationMinutes")
		}
		if f := c.Object("features", true); f != nil {
			f.Strict("netShield", "vpnAccelerator", "moderateNat")
			s.Certificate.Features.NetShield = f.Int("netShield", schema.IntOpt{Required: true, Min: &zero, Max: &two})
			requireBool(f, "vpnAccelerator", &s.Certificate.Features.VPNAccelerator)
			requireBool(f, "moderateNat", &s.Certificate.Features.ModerateNAT)
		}
	}
	if sv := n.Object("servers", true); sv != nil {
		sv.Strict("exitCountries", "entryCountries", "cities", "requireFeatures", "excludeFeatures",
			"maxLoad", "maxScore", "switchMarginPercent", "switchConfirmChecks")
		s.Servers.ExitCountries = countries(sv, "exitCountries")
		s.Servers.EntryCountries = countries(sv, "entryCountries")
		s.Servers.Cities = list(sv, "cities", false)
		s.Servers.RequireFeatures = features(sv, "requireFeatures", false)
		s.Servers.ExcludeFeatures = features(sv, "excludeFeatures", true)
		s.Servers.MaxLoad = sv.Int("maxLoad", schema.IntOpt{Required: true, Min: &zero, Max: &hundred})
		maxScore := 999
		s.Servers.MaxScore = sv.Int("maxScore", schema.IntOpt{Default: &maxScore, Min: &zero, Max: &maxScore})
		s.Servers.SwitchMarginPercent = sv.Int("switchMarginPercent", schema.IntOpt{Required: true, Min: &zero, Max: &hundred})
		s.Servers.SwitchConfirmChecks = sv.Int("switchConfirmChecks", schema.IntOpt{Required: true, Min: &one, Max: &ten})
	}
	p.parseWireGuard(n.Object("wireguard", false))
	s.Label = defaultLabel
	if n.Has("label") {
		if s.Label, _ = n.String("label", required); s.Label != "" && !strings.HasPrefix(s.Label, "{flag}") {
			n.Addf("label", "must start with {flag}")
		}
	}
	return p
}

func (p *Proton) parseWireGuard(wg *schema.Node) {
	wg.Strict("port", "dns", "mtu", "allowedIPs", "keepalive")
	w := &p.WireGuard
	defPort := 51820
	if w.Port = wg.Int("port", schema.IntOpt{Default: &defPort}); !slices.Contains(wgPorts, w.Port) {
		wg.Addf("port", "must be one of 443|88|1224|51820|500|4500")
	}
	w.DNS = []string{"10.2.0.1"}
	if wg.Has("dns") {
		w.DNS = wg.StringList("dns", required)
	}
	for i, d := range w.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			wg.Addf(fmt.Sprintf("dns.%d", i), "must be an IP address")
		}
	}
	minMTU, maxMTU, defMTU := 576, 1500, 1420
	w.MTU = wg.Int("mtu", schema.IntOpt{Default: &defMTU, Min: &minMTU, Max: &maxMTU})
	if wg.Has("allowedIPs") {
		w.AllowedIPs = wg.StringList("allowedIPs", required)
		for i, a := range w.AllowedIPs {
			if _, err := netip.ParsePrefix(a); err != nil {
				wg.Addf(fmt.Sprintf("allowedIPs.%d", i), "must be a CIDR prefix")
			}
		}
	}
	zero, maxKeepalive := 0, 65535
	w.Keepalive = wg.Int("keepalive", schema.IntOpt{Default: &zero, Min: &zero, Max: &maxKeepalive})
}

// requireBool reads the required boolean at key into dst.
func requireBool(n *schema.Node, key string, dst *bool) {
	if !n.Has(key) {
		n.Addf(key, "required")
	}
	*dst = n.Bool(key, false)
}

// list is a string list at key, [] when absent.
func list(n *schema.Node, key string, req bool) []string {
	if !n.Has(key) {
		if req {
			n.Addf(key, "required")
		}
		return []string{}
	}
	return n.StringList(key, schema.StrOpt{Required: true, Trim: true, MinLen: 1, AllowEmptyList: true})
}

func countries(n *schema.Node, key string) []string {
	out := list(n, key, false)
	for i, c := range out {
		if !countryPattern.MatchString(c) {
			n.Addf(fmt.Sprintf("%s.%d", key, i), "must be an upper-case ISO2 code")
		}
	}
	return out
}

func features(n *schema.Node, key string, req bool) []string {
	out := list(n, key, req)
	for i, f := range out {
		if _, ok := featureBits[f]; !ok {
			n.Addf(fmt.Sprintf("%s.%d", key, i), "must be one of secureCore|tor|p2p|streaming|ipv6")
		}
	}
	return out
}

// ID implements provider.Provider.
func (p *Proton) ID() string { return p.id }

// Kind implements provider.Provider.
func (*Proton) Kind() candidate.Kind { return candidate.KindWireGuard }
