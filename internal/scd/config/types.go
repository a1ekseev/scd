// Package config loads and validates the scd config file (01 §2) and
// defines its normalized form, which print-config prints as JSON.
package config

import (
	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/labelfilter"
)

// AppConfig is the normalized scd configuration. Field order is the
// print-config JSON order; optional values that were not configured are
// empty and omitted from JSON.
type AppConfig struct {
	Subscriptions []SubscriptionConfig `json:"subscriptions"`
	Runtime       RuntimeConfig        `json:"runtime"`
	Logging       LoggingConfig        `json:"logging"`
	Resources     ResourceConfig       `json:"resources"`
	StatusServer  StatusServerConfig   `json:"statusServer"`
}

// SubscriptionConfig is one subscription and the Xray target it is applied to.
type SubscriptionConfig struct {
	ID string `json:"id"`
	// Input is "-" (stdin), an http(s) URL or an absolute file path.
	Input          string `json:"input"`
	Enabled        bool   `json:"enabled"`
	Format         string `json:"format"` // auto|plain|base64
	FetchTimeoutMs int    `json:"fetchTimeoutMs"`
	// AllowInsecure is the policy for links with allowInsecure=1 and neither
	// pcs nor vcn (D15): "deny" (skip the link, default) or "allow".
	AllowInsecure string `json:"allowInsecure"`
	// FetchUserAgent is empty when not configured (the fetcher then uses its
	// default, D27 O9).
	FetchUserAgent string `json:"fetchUserAgent,omitempty"`
	// FetchHeaders are extra request headers (D33); nil when not configured.
	// Values are printed as "<redacted>".
	FetchHeaders inputsrc.Headers           `json:"fetchHeaders,omitempty"`
	Filters      *SubscriptionFiltersConfig `json:"filters,omitempty"`
	Target       SubscriptionTargetConfig   `json:"target"`
}

// SubscriptionFiltersConfig selects manifest entries by country and label.
// It is printed by MarshalJSON with the label filter keys labelIncludeRegex
// and labelExcludeRegex in place of Label.
type SubscriptionFiltersConfig struct {
	// CountryAllowlist holds upper-case ISO2 codes; nil or empty means no
	// country filter.
	CountryAllowlist []string           `json:"countryAllowlist,omitempty"`
	Label            labelfilter.Filter `json:"-"`
}

// MarshalJSON prints the filters in config form: countryAllowlist,
// labelIncludeRegex, labelExcludeRegex; empty keys are omitted.
func (f SubscriptionFiltersConfig) MarshalJSON() ([]byte, error) {
	return jsonout.Marshal(struct {
		CountryAllowlist []string `json:"countryAllowlist,omitempty"`
		Include          []string `json:"labelIncludeRegex,omitempty"`
		Exclude          []string `json:"labelExcludeRegex,omitempty"`
	}{f.CountryAllowlist, f.Label.IncludeSrc, f.Label.ExcludeSrc})
}

// SubscriptionTargetConfig describes the Xray instance (gRPC API address)
// and what scd manages and monitors there.
type SubscriptionTargetConfig struct {
	Address              string                      `json:"address"`
	TimeoutMs            int                         `json:"timeoutMs"`
	FixedOutbounds       []string                    `json:"fixedOutbounds"`
	FixedInbounds        []string                    `json:"fixedInbounds"`
	FixedRouting         []string                    `json:"fixedRouting"`
	VisionUDP443Override bool                        `json:"visionUdp443Override"`
	InboundSocks         *InboundSocksConfig         `json:"inboundSocks,omitempty"`
	Monitor              TargetMonitorConfig         `json:"monitor"`
	BalancerMonitor      TargetBalancerMonitorConfig `json:"balancerMonitor"`
	BalancerSelection    BalancerSelectionConfig     `json:"balancerSelection"`
	// ObservatorySubjectSelectorPrefix is prepended to generated outbound
	// tags; empty when not configured.
	ObservatorySubjectSelectorPrefix string `json:"observatorySubjectSelectorPrefix,omitempty"`
}

// InboundSocksConfig configures the generated per-tunnel SOCKS inbounds.
type InboundSocksConfig struct {
	Listen    string    `json:"listen"`
	PortRange PortRange `json:"portRange"`
}

// PortRange is an inclusive port range.
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// TargetMonitorConfig configures the per-tunnel monitor.
type TargetMonitorConfig struct {
	Enabled     bool                        `json:"enabled"`
	Schedule    string                      `json:"schedule,omitempty"`
	MaxParallel int                         `json:"maxParallel"`
	Retry       RetryConfig                 `json:"retry"`
	Request     *TargetMonitorRequestConfig `json:"request,omitempty"`
}

// RetryConfig is a probe retry policy.
type RetryConfig struct {
	Attempts int `json:"attempts"`
	DelayMs  int `json:"delayMs"`
}

// TargetMonitorRequestConfig is an HTTP probe request.
type TargetMonitorRequestConfig struct {
	URL            string `json:"url"`
	Method         string `json:"method"` // GET|HEAD|POST
	ExpectedStatus int    `json:"expectedStatus"`
	TimeoutMs      int    `json:"timeoutMs"`
}

// TargetBalancerMonitorConfig configures the balancer monitor (probe via
// an external SOCKS5 and optional remote push).
type TargetBalancerMonitorConfig struct {
	Enabled    bool                           `json:"enabled"`
	Schedule   string                         `json:"schedule,omitempty"`
	Socks5     *BalancerMonitorSocks5Config   `json:"socks5,omitempty"`
	Request    *TargetMonitorRequestConfig    `json:"request,omitempty"`
	Retry      RetryConfig                    `json:"retry"`
	RemotePing TargetBalancerRemotePingConfig `json:"remotePing"`
}

// BalancerMonitorSocks5Config is the SOCKS5 endpoint the balancer is probed through.
type BalancerMonitorSocks5Config struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// TargetBalancerRemotePingConfig configures the remote push of balancer
// monitor results.
type TargetBalancerRemotePingConfig struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url,omitempty"`
	TimeoutMs int    `json:"timeoutMs"`
	ViaSocks  bool   `json:"viaSocks"`
}

// BalancerSelectionConfig configures the read-only balancer selection view
// of the dashboard (D22, 10 §6.5).
type BalancerSelectionConfig struct {
	Enabled bool `json:"enabled"`
	// BalancerTag is routing.balancers[].tag in Xray (not the rule tag).
	BalancerTag string `json:"balancerTag,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	// Observatory enables ObservatoryService queries (default true).
	Observatory bool `json:"observatory"`
	// Strategy is an optional UI hint: leastPing|leastLoad|random|roundRobin.
	Strategy string `json:"strategy,omitempty"`
}

// RuntimeConfig selects one-shot or scheduled operation.
type RuntimeConfig struct {
	Mode     string `json:"mode"` // run-once|daemon
	Schedule string `json:"schedule,omitempty"`
}

// LoggingConfig configures the stderr logger (D27 O10).
type LoggingConfig struct {
	Level  string `json:"level"`  // fatal|error|warn|info|debug|trace|silent
	Format string `json:"format"` // json|pretty
}

// ResourceConfig selects which Xray resource kinds scd manages.
type ResourceConfig struct {
	Outbounds ResourceToggle `json:"outbounds"`
	Inbounds  ResourceToggle `json:"inbounds"`
	Routing   ResourceToggle `json:"routing"`
}

// ResourceToggle enables one resource kind.
type ResourceToggle struct {
	Enabled bool `json:"enabled"`
}

// StatusServerConfig configures the status dashboard and JSON API.
type StatusServerConfig struct {
	Enabled bool `json:"enabled"`
	// Listen is "host:port"; it may be empty when the server is disabled.
	Listen          string                       `json:"listen"`
	InstanceID      string                       `json:"instanceId,omitempty"`
	RemoteInstances []RemoteStatusInstanceConfig `json:"remoteInstances"`
	RuntimeState    RuntimeStateConfig           `json:"runtimeState"`
}

// RuntimeStateConfig configures /api/runtime-state.
type RuntimeStateConfig struct {
	Enabled        bool `json:"enabled"`
	IncludeRaw     bool `json:"includeRaw"`
	IncludeSecrets bool `json:"includeSecrets"`
}

// RemoteStatusInstanceConfig is another scd instance shown on the dashboard.
type RemoteStatusInstanceConfig struct {
	ID        string `json:"id"`
	APIURL    string `json:"apiUrl"`
	Schedule  string `json:"schedule"`
	TimeoutMs int    `json:"timeoutMs"`
}

// Loaded is a validated config with the absolute path of its file.
type Loaded struct {
	Path   string
	Config AppConfig
}
