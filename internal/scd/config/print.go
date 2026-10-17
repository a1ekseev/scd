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
// print it: SubscriptionConfig with fetchHeaders as a plain map, so the
// values can be shown (includeSecrets) or "<redacted>". Its fields mirror
// SubscriptionConfig in the same order.
type PrintedSubscription struct {
	ID             string                     `json:"id"`
	Input          string                     `json:"input"`
	Enabled        bool                       `json:"enabled"`
	Format         string                     `json:"format"`
	FetchTimeoutMs int                        `json:"fetchTimeoutMs"`
	AllowInsecure  string                     `json:"allowInsecure"`
	FetchUserAgent string                     `json:"fetchUserAgent,omitempty"`
	FetchHeaders   map[string]string          `json:"fetchHeaders,omitempty"`
	Filters        *SubscriptionFiltersConfig `json:"filters,omitempty"`
	Target         SubscriptionTargetConfig   `json:"target"`
}

// Printed returns s for printing: unless includeSecrets, the input is
// redacted with inputsrc.RedactInput, the target URLs with RedactSecrets
// and the fetchHeaders values are "<redacted>".
func (s SubscriptionConfig) Printed(includeSecrets bool) PrintedSubscription {
	p := PrintedSubscription{
		ID: s.ID, Input: s.Input, Enabled: s.Enabled, Format: s.Format, FetchTimeoutMs: s.FetchTimeoutMs,
		AllowInsecure: s.AllowInsecure, FetchUserAgent: s.FetchUserAgent,
		FetchHeaders: s.FetchHeaders.Printed(includeSecrets), Filters: s.Filters, Target: s.Target,
	}
	if !includeSecrets {
		p.Input = inputsrc.RedactInput(s.Input)
		p.Target = s.Target.RedactSecrets()
	}
	return p
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
