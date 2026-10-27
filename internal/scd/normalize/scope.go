package normalize

import (
	"strings"

	"github.com/a1ekseev/scd/internal/common/vless"
)

const reasonCombo = "Unsupported VLESS type/security/flow combination."

// scope is the transport and security of a link inside the D26 scope.
type scope struct {
	// network is tcp (also for type=raw), ws or xhttp.
	network string
	// security is tls, reality or none (none only with ML-KEM).
	security string
}

// resolveScope maps the type and security parameters onto the D26 scope
// (11 §1, §3). It fails for transports outside the scope (grpc, httpupgrade,
// kcp, …), unknown or removed security (xtls), ws+reality (Xray: "REALITY
// only supports RAW, XHTTP and gRPC") and security=none without ML-KEM
// (11 §3 #15: checked here because conf accepts it for private addresses).
func resolveScope(l vless.Link, mlkem bool) (scope, bool) {
	typ, _ := l.Get("type")
	sec, _ := l.Get("security")
	sc := scope{network: strings.ToLower(typ), security: strings.ToLower(sec)}
	switch sc.network {
	case "", "raw":
		sc.network = "tcp"
	case "tcp", "ws", "xhttp":
	default:
		return scope{}, false
	}
	switch sc.security {
	case "":
		sc.security = "none"
	case "none", "tls", "reality":
	default:
		return scope{}, false
	}
	if sc.network == "ws" && sc.security == "reality" {
		return scope{}, false
	}
	if sc.security == "none" && !mlkem {
		return scope{}, false
	}
	return sc, true
}

// comboDetails lists type, security and flow as written in the link (TS
// parity: a missing type reads "tcp", missing security and flow read "").
func comboDetails(l vless.Link) []string {
	typ, ok := l.Get("type")
	if !ok {
		typ = "tcp"
	}
	sec, _ := l.Get("security")
	flow, _ := l.Get("flow")
	return []string{"type=" + typ, "security=" + sec, "flow=" + flow}
}

// profile names the outbound profile (11 §3 «Профили и теги»):
// <network>-<tls|reality|mlkem>, plus "-vision" only for REALITY with a kept
// vision flow. ML-KEM over tls/reality does not change the profile.
func (sc scope) profile(flow string) string {
	name := sc.security
	if name == "none" {
		name = "mlkem"
	}
	p := sc.network + "-" + name
	if sc.security == "reality" && flow != "" {
		p += "-vision"
	}
	return p
}
