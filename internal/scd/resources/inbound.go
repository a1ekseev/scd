// Package resources builds the SOCKS inbounds and routing rules scd adds to
// Xray (D19) and describes the outbounds, inbounds and rules Xray reports
// for /api/runtime-state (03 §6.4), reading the xray-core types with their
// correct fields (D6) and redacting secrets (03 §6.5, D33).
package resources

import (
	"encoding/json"
	"fmt"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"

	"github.com/a1ekseev/scd/internal/scd/topology"
)

// inboundJSON is the Xray JSON config of a tunnel's SOCKS inbound.
type inboundJSON struct {
	Tag      string        `json:"tag"`
	Listen   string        `json:"listen"`
	Port     int           `json:"port"`
	Protocol string        `json:"protocol"`
	Settings socksSettings `json:"settings"`
}

type socksSettings struct {
	Auth string `json:"auth"`
	UDP  bool   `json:"udp"`
}

// Inbound returns the no-auth, UDP-enabled SOCKS inbound of t, built from
// its Xray JSON config with infra/conf (D19). An IP listen address becomes
// IPOrDomain.ip (IPv6 included); conf rejects a domain other than localhost,
// returned as "invalid inbound <tag>: <err>".
func Inbound(t topology.Tunnel) (*core.InboundHandlerConfig, error) {
	raw, err := json.Marshal(inboundJSON{
		Tag:      t.InboundTag,
		Listen:   t.Listen,
		Port:     t.Port,
		Protocol: "socks",
		Settings: socksSettings{Auth: "noauth", UDP: true},
	})
	if err != nil {
		return nil, fmt.Errorf("inbound %s: %w", t.InboundTag, err)
	}
	var dc conf.InboundDetourConfig
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, fmt.Errorf("invalid inbound %s: %w", t.InboundTag, err)
	}
	h, err := dc.Build()
	if err != nil {
		return nil, fmt.Errorf("invalid inbound %s: %w", t.InboundTag, err)
	}
	return h, nil
}
