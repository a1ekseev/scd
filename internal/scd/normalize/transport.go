package normalize

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/a1ekseev/scd/internal/common/vless"
)

// Skip reasons of applyTransport (verbatim, 4.5).
const (
	reasonHeartbeat = "ws heartbeatPeriod must be a non-negative integer."
	reasonExtra     = "xhttp extra must be a JSON object."
	reasonFinalMask = "fm (finalmask) must be a JSON object."
)

// applyTransport fills the transport fields of n for n.Network and the
// finalmask (11 §4). Values are mapped, not validated: conf.Build rejects bad
// ones in the manifest (11 §5). Only what would break the JSON outbound
// itself is skipped here: a ws heartbeatPeriod that is not a uint32 (D33) and
// an extra/fm that is not a JSON object (they are embedded as raw JSON).
// Parameters of the other transports are dropped silently (O16); an empty
// value is an unset parameter.
func applyTransport(l vless.Link, n *Normalized) *vless.Skip {
	get := func(key string) string {
		v, _ := l.Get(key)
		return v
	}
	skip := func(reason, detail string) *vless.Skip {
		label := l.Label
		return &vless.Skip{
			Line: l.Line, Raw: l.Raw, Label: &label, Code: vless.CodeUnsupportedValue,
			Reason: reason, Details: []string{detail},
		}
	}

	switch n.Network {
	case "tcp":
		if ht, ok := l.Get("headertype"); ok {
			n.HeaderType = &ht // "" and "none" are kept, as in TS
			if ht == "http" {
				// Comma lists are split by the outbound builder (4.6).
				n.Host = get("host")
				n.Path = get("path")
			}
		}
	case "ws":
		n.Host = get("host")
		n.Path = get("path") // may be empty; conf extracts ?ed=
		if v := get("heartbeatperiod"); v != "" {
			hb, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return skip(reasonHeartbeat, "heartbeatPeriod="+v)
			}
			n.HeartbeatPeriod = uint32(hb)
		}
	case "xhttp":
		n.Host = get("host")
		n.Path = get("path")
		n.XHTTPMode = get("mode") // conf rejects unknown modes
		if v := get("extra"); v != "" {
			if !isJSONObject(v) {
				return skip(reasonExtra, "extra="+truncate(v, 40))
			}
			n.XHTTPExtra = json.RawMessage(v)
		}
	}

	if v := get("fm"); v != "" {
		if !isJSONObject(v) {
			return skip(reasonFinalMask, "fm="+truncate(v, 40))
		}
		n.FinalMask = json.RawMessage(v)
	}
	return nil
}

// isJSONObject reports whether s is a single valid JSON value that is an
// object.
func isJSONObject(s string) bool {
	b := []byte(s)
	return json.Valid(b) && bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte("{"))
}
