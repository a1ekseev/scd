package normalize

import (
	"strings"

	"github.com/a1ekseev/scd/internal/common/vless"
)

const reasonPbk = `REALITY VLESS requires parameter "pbk".`

// pbkReplacer turns a standard-base64 key into the base64url without padding
// that conf accepts for realitySettings.password (11 §4 pbk).
var pbkReplacer = strings.NewReplacer("=", "", "+", "-", "/", "_")

// applySecurity fills the TLS/REALITY fields of n for n.Security. Values are
// mapped, not validated: conf.Build rejects bad ones in the manifest (11 §5).
// Only an empty REALITY pbk is skipped here; parameters of the other
// security are dropped silently (O16).
func applySecurity(l vless.Link, n *Normalized) *vless.Skip {
	get := func(key string) string {
		v, _ := l.Get(key)
		return v
	}
	switch n.Security {
	case "tls":
		n.SNI = get("sni")
		n.Fingerprint = strings.ToLower(get("fp"))
		if alpn := splitList(get("alpn")); alpn != nil {
			n.ALPN = alpn
		}
		n.PinnedPeerCertSha256 = splitList(get("pcs"))
		n.VerifyPeerCertByName = splitList(get("vcn"))
		n.ECHConfigList = get("ech")
		n.CipherSuites = get("cs") // D33
	case "reality":
		pbk := get("pbk")
		if pbk == "" {
			label := l.Label
			return &vless.Skip{
				Line: l.Line, Raw: l.Raw, Label: &label, Code: vless.CodeMissingRequiredField, Reason: reasonPbk,
			}
		}
		n.SNI = get("sni")
		n.Fingerprint = strings.ToLower(get("fp"))
		n.PublicKey = pbkReplacer.Replace(pbk)
		n.ShortID = get("sid") // empty is valid (D17)
		n.SpiderX = get("spx")
		if n.SpiderX == "" {
			n.SpiderX = "/" // D13
		}
		n.Mldsa65Verify = get("pqv")
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
