// Package providertest registers fake provider types for tests. Only test
// files import it; internal/wgp/provider/all never does.
//
//   - "fake": keys kind (wireguard|masque) and lines; identification.fake
//     with a required timezone; its outputs require candidates.
//   - "fakedir": key dir; no identification; candidates is an error.
//   - "fakelogin": like "fakedir" without keys, and Interactive: Login
//     saves {"session":"<first tty line>"}, Logout requires a record and
//     saves {}.
//
// Tick returns the configured lines.
package providertest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// Identification is the parsed identification.fake.
type Identification struct {
	Timezone string `json:"timezone"`
}

// Fake is a provider of either fake type.
type Fake struct {
	id    string
	typ   string
	Env   provider.Env   `json:"-"`
	KindV candidate.Kind `json:"kind,omitempty"`
	Lines []string       `json:"lines,omitempty"`
	Dir   string         `json:"dir,omitempty"`
}

// ID implements provider.Provider.
func (f *Fake) ID() string { return f.id }

// Kind implements provider.Provider.
func (f *Fake) Kind() candidate.Kind {
	if f.KindV == "" {
		return candidate.KindWireGuard
	}
	return f.KindV
}

// Tick implements provider.Provider: it returns the configured lines.
func (f *Fake) Tick(context.Context, provider.Store) (provider.Result, error) {
	return provider.Result{Lines: f.Lines, Status: wgpstatus.Provider{
		ID: f.id, Output: f.Env.Output.ID, Type: f.typ, State: wgpstatus.StateOK, Issued: len(f.Lines),
	}}, nil
}

// Login is a fake that can sign in.
type Login struct{ Fake }

// Login implements provider.Interactive.
func (*Login) Login(_ context.Context, tty provider.TTY, s provider.Store) error {
	line, err := bufio.NewReader(tty.In).ReadString('\n')
	if err != nil {
		return err
	}
	b, err := json.Marshal(map[string]string{"session": line[:len(line)-1]})
	if err != nil {
		return err
	}
	return s.Save(b)
}

// Logout implements provider.Interactive.
func (*Login) Logout(_ context.Context, s provider.Store) error {
	b, err := s.Load()
	if err != nil {
		return err
	}
	if b == nil {
		return errors.New("not logged in")
	}
	return s.Save([]byte(`{}`))
}

func init() {
	provider.Register("fake", provider.Factory{
		New: func(n *schema.Node, env provider.Env) provider.Provider {
			n.Strict(slices.Concat(provider.CommonKeys, []string{"kind", "lines"})...)
			kind := n.Enum("kind", []string{string(candidate.KindWireGuard), string(candidate.KindMasque)}, "")
			return &Fake{
				id: env.ID, typ: "fake", Env: env, KindV: candidate.Kind(kind),
				Lines: n.StringList("lines", schema.StrOpt{Trim: true, MinLen: 1}),
			}
		},
		Identification: func(n *schema.Node) any {
			n.Strict("timezone")
			return Identification{Timezone: provider.Timezone(n, "timezone")}
		},
		Candidates: true,
	})
	provider.Register("fakedir", provider.Factory{
		New: func(n *schema.Node, env provider.Env) provider.Provider {
			n.Strict(slices.Concat(provider.CommonKeys, []string{"dir"})...)
			dir, _ := n.String("dir", schema.StrOpt{Required: true, Trim: true, MinLen: 1})
			return &Fake{id: env.ID, typ: "fakedir", Env: env, Dir: dir}
		},
	})
	provider.Register("fakelogin", provider.Factory{
		New: func(n *schema.Node, env provider.Env) provider.Provider {
			n.Strict(provider.CommonKeys...)
			return &Login{Fake{id: env.ID, typ: "fakelogin", Env: env}}
		},
	})
}
