// Package provider is the wgp provider registry and the interfaces between
// the wgp core and its providers. A provider type is a package that calls
// Register from its init function; internal/wgp/provider/all imports every
// such package. The core never names a provider type.
package provider

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"
	_ "time/tzdata" // IANA names in config are checked without the image zoneinfo

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
)

// Provider is one configured provider: it keeps its accounts and keys and
// returns the candidate lines of its output on every tick.
//
// print-config prints the JSON encoding of a Provider after the common
// keys, so it must encode as a JSON object of its config keys (no secrets).
type Provider interface {
	// ID is the provider id from the config.
	ID() string
	// Kind is the kind of every line the provider publishes.
	Kind() candidate.Kind
	// Tick runs one scheduled pass with the provider's state record.
	Tick(ctx context.Context, s Store) (Result, error)
}

// Result is the outcome of one tick.
type Result struct {
	// Lines are the candidate lines of the output.
	Lines []string
	// Status is the provider entry of status.json.
	Status wgpstatus.Provider
	// Warnings are logged by the core; they never hold secrets.
	Warnings []string
	// Events are logged by the core in order, also when Tick returns an
	// error or ctx is done.
	Events []Event
}

// Event is one decision or state change of a provider; the core logs it
// with the provider id, type and output id. Attrs never hold secrets: only
// names, counts, times, scores and API status codes.
type Event struct {
	// Name is the snake_case event name; it names no provider.
	Name string
	// Msg is one sentence ending with ".".
	Msg string
	// Level is slog.LevelInfo, slog.LevelWarn or slog.LevelDebug.
	Level slog.Level
	// Slot is the 1-based candidate slot, 0 for the whole provider.
	Slot int
	// Attrs are the event fields; the core drops the keys it sets itself.
	Attrs []slog.Attr
}

// Interactive is implemented by providers that need an owner login
// (`scd wgp login|logout`). The core detects it with a type assertion.
type Interactive interface {
	// Login signs in interactively through tty and saves the session to s.
	Login(ctx context.Context, tty TTY, s Store) error
	// Logout revokes what the provider issued and closes the session.
	Logout(ctx context.Context, s Store) error
}

// TTY is the owner terminal of an interactive command.
type TTY struct {
	In  io.Reader
	Out io.Writer
	// HVToken is login --hv-token: a human verification token from an
	// earlier attempt, empty when not given.
	HVToken string
}

// Store is the encrypted state record of one provider.
type Store interface {
	// Load returns the record, nil when the provider has none yet.
	Load() ([]byte, error)
	// Save replaces the record atomically.
	Save(data []byte) error
}

// Output is the wgp output a provider feeds.
type Output struct {
	ID string
	// Candidates is the configured number of candidates, 0 for a type
	// without the key.
	Candidates int
}

// API holds the common API request settings (api.*).
type API struct {
	TimeoutMs int `json:"timeoutMs"`
	Retry     struct {
		Attempts   int `json:"attempts"`
		MinDelayMs int `json:"minDelayMs"`
		MaxDelayMs int `json:"maxDelayMs"`
	} `json:"retry"`
}

// Env is what the core passes to a Factory besides the provider subtree.
type Env struct {
	// ID is the provider id.
	ID string
	// Output is the output fed by the provider; zero for a disabled
	// provider without one.
	Output Output
	// Identification is the value Factory.Identification returned for
	// identification.<type>, nil for a type without it.
	Identification   any
	DeviceNamePrefix string
	// RevokeOldAfterMinutes is keys.revokeOldAfterMinutes, 0 when unset; a
	// type that needs it reports it as required.
	RevokeOldAfterMinutes int
	API                   API
}

// Factory builds the providers of one type.
type Factory struct {
	// New parses the provider subtree n (it must call n.Strict with
	// CommonKeys and its own keys) and returns the provider. Problems are
	// recorded on n; the result is not used when any problem was recorded.
	New func(n *schema.Node, env Env) Provider
	// Identification parses identification.<type> (n is nil when the
	// object is absent; the core has reported that). Nil when the type has
	// no identification.
	Identification func(n *schema.Node) any
	// Candidates is true when the outputs of the type require the
	// candidates key; otherwise the key is an error.
	Candidates bool
}

// CommonKeys are the provider keys the core parses for every type.
var CommonKeys = []string{"id", "type", "enabled", "schedule"}

// registry is written only by init functions, then only read.
var registry = map[string]Factory{}

// Register adds a provider type. It panics on an empty name, a nil New or a
// type registered twice: registration happens at init, so this is a
// programming error.
func Register(name string, f Factory) {
	if name == "" || f.New == nil {
		panic(fmt.Sprintf("provider: invalid registration of type %q", name))
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("provider: type %q registered twice", name))
	}
	registry[name] = f
}

// Lookup returns the factory of a registered type.
func Lookup(name string) (Factory, bool) {
	f, ok := registry[name]
	return f, ok
}

// Types returns the registered type names in sorted order.
func Types() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Timezone returns the required IANA time zone name at key, checked with
// time.LoadLocation against the embedded tzdata.
func Timezone(n *schema.Node, key string) string {
	tz, ok := n.String(key, schema.StrOpt{Required: true, Trim: true, MinLen: 1})
	if !ok {
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
		n.Addf(key, "must be an IANA time zone name")
		return ""
	}
	return tz
}
