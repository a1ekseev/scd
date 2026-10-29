package proton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// errNeedsLogin means there is no session or the API rejected its refresh:
// the provider state is needs_login until the owner signs in again.
var errNeedsLogin = errors.New("proton: no valid session, sign in again with scd wgp login")

// Proton is one configured Proton VPN provider. Its JSON is its config
// keys (print-config); the session and the keys live only in the state
// record.
type Proton struct {
	id  string
	api *api
	// out is the output the provider feeds; its id is part of DeviceName.
	out provider.Output
	// prefix is deviceNamePrefix.
	prefix string
	// revokeAfter is keys.revokeOldAfterMinutes.
	revokeAfter time.Duration
	now         func() time.Time
	// rotate is keys.rotateOnStart until the rotation of this process ran.
	rotate bool
	// better counts, per candidate slot, the ticks in a row with a better
	// free server (servers.switch*); in memory only.
	better map[int]int

	settings
	// pinged runs the ping of the client once per process, before its
	// first API request.
	pinged sync.Once

	// mu guards the session and serializes refreshes.
	mu sync.Mutex
	// sess is the session, nil until loaded from the state record.
	sess *tokens
	// dead is set when the API rejected a refresh.
	dead bool
	// events are the session events since the last takeEvents (mu held).
	events []provider.Event
}

var _ provider.Interactive = (*Proton)(nil)

// tokens is the session in the state record.
type tokens struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// sessionKey is the key of the session in the state record.
const sessionKey = "session"

// newProton returns the provider id with its API client; cfg gives RootCAs,
// Dial and API.
func newProton(id string, ident Identification, cfg transport.Config) (*Proton, error) {
	a, err := newAPI(ident, cfg)
	if err != nil {
		return nil, err
	}
	return &Proton{id: id, api: a, now: time.Now, better: map[int]int{}}, nil
}

// send pings once per process, then sends c as is.
func (p *Proton) send(ctx context.Context, c call) (transport.Response, error) {
	p.pinged.Do(func() { _, _ = p.api.do(ctx, call{method: http.MethodGet, path: "/tests/ping"}) })
	return p.api.do(ctx, c)
}

// authed sends c with the session. On a 401 it refreshes the session once
// (one refresh for all concurrent 401s of the same token) and repeats c.
func (p *Proton) authed(ctx context.Context, s provider.Store, c call) (transport.Response, error) {
	tk, err := p.session(s)
	if err != nil {
		return transport.Response{}, err
	}
	c.uid, c.token = tk.UID, tk.AccessToken
	resp, err := p.send(ctx, c)
	if resp.Status != http.StatusUnauthorized {
		return resp, err
	}
	if tk, err = p.refresh(ctx, s, tk.AccessToken); err != nil {
		return transport.Response{}, err
	}
	c.uid, c.token = tk.UID, tk.AccessToken
	return p.send(ctx, c)
}

// session returns the session, loading it from s on first use.
func (p *Proton) session(s provider.Store) (tokens, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		return tokens{}, errNeedsLogin
	}
	if p.sess == nil {
		rec, err := loadRecord(s)
		if err != nil {
			return tokens{}, err
		}
		var tk tokens
		if raw, ok := rec[sessionKey]; ok {
			if err := json.Unmarshal(raw, &tk); err != nil {
				return tokens{}, errors.New("proton: state record: bad session")
			}
		}
		if tk.UID == "" || tk.AccessToken == "" || tk.RefreshToken == "" {
			return tokens{}, errNeedsLogin
		}
		p.sess = &tk
	}
	return *p.sess, nil
}

// refresh renews the session whose access token got a 401. The request goes
// with that old Bearer, as the client does. When another caller already
// replaced that token, its session is returned without a new refresh. The
// rotated tokens are saved before they are used. 400, 422 and code 10013
// mean the session is dead: errNeedsLogin, now and on every later call.
func (p *Proton) refresh(ctx context.Context, s provider.Store, old string) (tokens, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead || p.sess == nil {
		return tokens{}, errNeedsLogin
	}
	if p.sess.AccessToken != old {
		return *p.sess, nil
	}
	resp, err := p.send(ctx, call{method: http.MethodPost, path: "/auth/refresh", uid: p.sess.UID, token: old, body: obj{
		{"ResponseType", "token"},
		{"GrantType", "refresh_token"},
		{"RefreshToken", p.sess.RefreshToken},
		{"RedirectURI", "http://protonmail.ch"},
	}})
	if err != nil {
		if se, ok := errors.AsType[*transport.StatusError](err); ok &&
			(se.Status == http.StatusBadRequest || se.Status == http.StatusUnprocessableEntity || se.Code == codeRefreshInvalid) {
			p.dead = true
			return tokens{}, fmt.Errorf("%w (refresh: %w)", errNeedsLogin, err)
		}
		return tokens{}, fmt.Errorf("proton: refresh: %w", err)
	}
	var r struct{ UID, AccessToken, RefreshToken string }
	if json.Unmarshal(resp.Body, &r) != nil || r.AccessToken == "" || r.RefreshToken == "" {
		return tokens{}, errors.New("proton: refresh: no tokens in the answer")
	}
	next := tokens{UID: p.sess.UID, AccessToken: r.AccessToken, RefreshToken: r.RefreshToken}
	if r.UID != "" {
		next.UID = r.UID
	}
	if err := saveSession(s, &next); err != nil {
		return tokens{}, fmt.Errorf("proton: save the refreshed session: %w", err)
	}
	p.sess = &next
	p.events = append(p.events, provider.Event{Name: "session_refreshed", Msg: "Session refreshed.", Level: slog.LevelInfo})
	return next, nil
}

// takeEvents returns and clears the session events.
func (p *Proton) takeEvents() []provider.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := p.events
	p.events = nil
	return ev
}

// setSession replaces the session after a login or a logout (nil).
func (p *Proton) setSession(tk *tokens) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sess, p.dead = tk, false
}

// loadRecord returns the state record as its top-level members, so that a
// session update keeps the rest of the record.
func loadRecord(s provider.Store) (map[string]json.RawMessage, error) {
	b, err := s.Load()
	if err != nil {
		return nil, err
	}
	rec := map[string]json.RawMessage{}
	if b != nil && json.Unmarshal(b, &rec) != nil {
		return nil, errors.New("proton: state record is not a JSON object")
	}
	return rec, nil
}

// saveSession writes tk, or removes the session when tk is nil, and keeps
// the rest of the record.
func saveSession(s provider.Store, tk *tokens) error {
	if tk == nil {
		return saveMember(s, sessionKey, nil)
	}
	return saveMember(s, sessionKey, tk)
}

// saveMember writes v as the record member key, or removes it when v is
// nil, and keeps the other members. There is one writer at a time: a tick
// runs alone, and a refresh runs inside a call of the tick or the login.
func saveMember(s provider.Store, key string, v any) error {
	rec, err := loadRecord(s)
	if err != nil {
		return err
	}
	delete(rec, key)
	if v != nil {
		if rec[key], err = json.Marshal(v); err != nil {
			return err
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.Save(b)
}
