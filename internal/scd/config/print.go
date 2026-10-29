package config

import "github.com/a1ekseev/scd/internal/common/inputsrc"

// RedactSecrets returns a copy of t with the monitor request URL, the
// balancer monitor request URL and the remote ping URL replaced by
// inputsrc.RedactURL (D33; print-config and runtime-state).
func (t SubscriptionTargetConfig) RedactSecrets() SubscriptionTargetConfig {
	if r := t.Monitor.Request; r != nil {
		c := *r
		c.URL = inputsrc.RedactURL(c.URL)
		t.Monitor.Request = &c
	}
	if r := t.BalancerMonitor.Request; r != nil {
		c := *r
		c.URL = inputsrc.RedactURL(c.URL)
		t.BalancerMonitor.Request = &c
	}
	if t.BalancerMonitor.RemotePing.URL != "" {
		t.BalancerMonitor.RemotePing.URL = inputsrc.RedactURL(t.BalancerMonitor.RemotePing.URL)
	}
	return t
}

// PrintedSubscription is a subscription as print-config and runtime-state
// print it: SubscriptionConfig with printed sources. Its fields mirror
// SubscriptionConfig in the same order.
type PrintedSubscription struct {
	ID      string                   `json:"id"`
	Enabled bool                     `json:"enabled"`
	Sources []PrintedSource          `json:"sources"`
	Target  SubscriptionTargetConfig `json:"target"`
}

// PrintedSource is SourceConfig with fetchHeaders as a plain map, so the
// values can be shown (includeSecrets) or "<redacted>". Its fields mirror
// SourceConfig in the same order.
type PrintedSource struct {
	ID             string               `json:"id"`
	Type           string               `json:"type"`
	Input          string               `json:"input"`
	Enabled        bool                 `json:"enabled"`
	Format         string               `json:"format,omitempty"`
	FetchTimeoutMs int                  `json:"fetchTimeoutMs,omitempty"`
	AllowInsecure  string               `json:"allowInsecure,omitempty"`
	FetchUserAgent string               `json:"fetchUserAgent,omitempty"`
	FetchHeaders   map[string]string    `json:"fetchHeaders,omitempty"`
	MaxInputBytes  int64                `json:"maxInputBytes"`
	PortRange      *PortRange           `json:"portRange,omitempty"`
	Filters        *SourceFiltersConfig `json:"filters,omitempty"`
	MaxActive      int                  `json:"maxActive,omitempty"`
	Selection      *SelectionConfig     `json:"selection,omitempty"`
	Monitor        *SourceMonitorConfig `json:"monitor,omitempty"`
	WireGuard      *WireGuardConfig     `json:"wireguard,omitempty"`
}

// Printed returns s for printing: unless includeSecrets, the input is
// redacted with inputsrc.RedactInput and the fetchHeaders values are
// "<redacted>".
func (s SourceConfig) Printed(includeSecrets bool) PrintedSource {
	p := PrintedSource{
		ID: s.ID, Type: s.Type, Input: s.Input, Enabled: s.Enabled, Format: s.Format, FetchTimeoutMs: s.FetchTimeoutMs,
		AllowInsecure: s.AllowInsecure, FetchUserAgent: s.FetchUserAgent,
		FetchHeaders: s.FetchHeaders.Printed(includeSecrets), MaxInputBytes: s.MaxInputBytes,
		PortRange: s.PortRange, Filters: s.Filters,
		MaxActive: s.MaxActive, Selection: s.Selection, Monitor: s.Monitor, WireGuard: s.WireGuard,
	}
	if !includeSecrets {
		p.Input = inputsrc.RedactInput(s.Input)
	}
	return p
}

// Printed returns s for printing: unless includeSecrets, the sources are
// redacted (SourceConfig.Printed) and the target URLs with RedactSecrets.
func (s SubscriptionConfig) Printed(includeSecrets bool) PrintedSubscription {
	p := PrintedSubscription{ID: s.ID, Enabled: s.Enabled, Sources: make([]PrintedSource, 0, len(s.Sources)), Target: s.Target}
	for _, src := range s.Sources {
		p.Sources = append(p.Sources, src.Printed(includeSecrets))
	}
	if !includeSecrets {
		p.Target = s.Target.RedactSecrets()
	}
	return p
}

// EnabledSources returns the enabled sources of s in config order.
func (s SubscriptionConfig) EnabledSources() []SourceConfig {
	var out []SourceConfig
	for _, src := range s.Sources {
		if src.Enabled {
			out = append(out, src)
		}
	}
	return out
}

// printedConfig is AppConfig with printed subscriptions; the shallower
// Subscriptions field hides the embedded one and keeps its JSON position.
type printedConfig struct {
	Subscriptions []PrintedSubscription `json:"subscriptions"`
	AppConfig
}

// Printed returns the config as print-config prints it, with the secrets
// of every subscription redacted unless includeSecrets (D33, final review
// I3).
func (c AppConfig) Printed(includeSecrets bool) any {
	subs := make([]PrintedSubscription, 0, len(c.Subscriptions))
	for _, s := range c.Subscriptions {
		subs = append(subs, s.Printed(includeSecrets))
	}
	return printedConfig{Subscriptions: subs, AppConfig: c}
}
