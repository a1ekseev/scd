package proton

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/provider/proton/srp"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// Proton API error codes.
const (
	codeWrongPassword  = 8002  // wrong password or 2FA code, or a locked login
	codeHumanVerify    = 9001  // human verification (CAPTCHA) required
	codeRefreshInvalid = 10013 // the refresh token is not valid
	codeBadHVToken     = 12087 // the human verification token is not valid
)

// totpBit is the TOTP bit of 2FA.Enabled.
const totpBit = 1

// Login signs in with SRP from the terminal: username, password and, when
// the account has it, a TOTP code. The password and the code are read
// without echo from a terminal and never stored. The session is saved only
// after the server proof matched (and 2FA passed). Then the requests the
// client sends after a login go out; their errors are ignored.
//
// Unlike the client, a CAPTCHA (code 9001) prints the verification link and
// a later login sends tty.HVToken in the x-pm-human-verification-* headers.
func (p *Proton) Login(ctx context.Context, tty provider.TTY, s provider.Store) error {
	in := bufio.NewReader(tty.In)
	user, err := readLine(tty, in, "Proton username: ", false)
	if err != nil {
		return err
	}
	password, err := readLine(tty, in, "Password: ", true)
	if err != nil {
		return err
	}
	defer clear(password)
	var hv []transport.Header
	if tty.HVToken != "" {
		hv = []transport.Header{
			{Name: "x-pm-human-verification-token", Value: tty.HVToken},
			{Name: "x-pm-human-verification-token-type", Value: "captcha"},
		}
	}

	resp, err := p.send(ctx, call{method: http.MethodPost, path: "/auth/info", header: hv, body: obj{{"Username", string(user)}}})
	if err != nil {
		return loginError(tty, "auth info", resp, err)
	}
	var info struct {
		Version                                    int
		Modulus, ServerEphemeral, Salt, SRPSession string
	}
	if json.Unmarshal(resp.Body, &info) != nil {
		return errors.New("proton: auth info: bad answer")
	}
	proofs, err := srp.NewProofs(srp.Challenge{
		Version: info.Version, Modulus: info.Modulus, ServerEphemeral: info.ServerEphemeral, Salt: info.Salt,
	}, password)
	clear(password)
	if err != nil {
		return fmt.Errorf("proton: srp: %w", err)
	}
	resp, err = p.send(ctx, call{method: http.MethodPost, path: "/auth", header: hv, body: obj{
		{"Username", string(user)},
		{"ClientEphemeral", base64.StdEncoding.EncodeToString(proofs.ClientEphemeral)},
		{"ClientProof", base64.StdEncoding.EncodeToString(proofs.ClientProof)},
		{"SRPSession", info.SRPSession},
	}})
	if err != nil {
		return loginError(tty, "auth", resp, err)
	}
	var auth struct {
		UID, AccessToken, RefreshToken, ServerProof string
		TwoFA                                       struct{ Enabled int } `json:"2FA"`
	}
	if json.Unmarshal(resp.Body, &auth) != nil {
		return errors.New("proton: auth: bad answer")
	}
	// Nothing of the answer is trusted before the server proves it knows
	// the verifier.
	sp, err := base64.StdEncoding.DecodeString(auth.ServerProof)
	if err != nil || proofs.VerifyServerProof(sp) != nil {
		return errors.New("proton: auth: the server proof does not match")
	}
	tk := tokens{UID: auth.UID, AccessToken: auth.AccessToken, RefreshToken: auth.RefreshToken}
	if tk.UID == "" || tk.AccessToken == "" || tk.RefreshToken == "" {
		return errors.New("proton: auth: no session in the answer")
	}
	if auth.TwoFA.Enabled != 0 {
		if auth.TwoFA.Enabled&totpBit == 0 {
			return errors.New("proton: the account needs a two-factor method other than TOTP, which is not supported")
		}
		code, err := readLine(tty, in, "TOTP code: ", true)
		if err != nil {
			return err
		}
		// shortcut: the code also lives in a Go string and the JSON body
		// until the GC reuses them; a code is valid for seconds only.
		resp, err = p.send(ctx, call{method: http.MethodPost, path: "/auth/2fa", uid: tk.UID, token: tk.AccessToken, body: obj{{"TwoFactorCode", string(code)}}})
		clear(code)
		if err != nil {
			return loginError(tty, "2fa", resp, err)
		}
	}
	if err := saveSession(s, &tk); err != nil {
		return fmt.Errorf("proton: save the session: %w", err)
	}
	p.setSession(&tk)
	p.afterLogin(ctx, s)
	_, _ = fmt.Fprintln(tty.Out, "Signed in.")
	return nil
}

// loginError explains a failed login step without the answer body.
func loginError(tty provider.TTY, step string, resp transport.Response, err error) error {
	se, ok := errors.AsType[*transport.StatusError](err)
	if !ok {
		return fmt.Errorf("proton: %s: %w", step, err)
	}
	switch se.Code {
	case codeHumanVerify:
		var d struct {
			Details struct{ HumanVerificationToken string }
		}
		_ = json.Unmarshal(resp.Body, &d)
		_, _ = fmt.Fprintf(tty.Out, "Human verification required. Open https://verify.proton.me/?methods=captcha&token=%s\n"+
			"solve the CAPTCHA, then run login again with --hv-token %s\n",
			url.QueryEscape(d.Details.HumanVerificationToken), d.Details.HumanVerificationToken)
		return fmt.Errorf("proton: %s: human verification required (code %d): run login again with --hv-token", step, se.Code)
	case codeBadHVToken:
		return fmt.Errorf("proton: %s: the human verification token is not valid (code %d): run login without --hv-token for a new one", step, se.Code)
	case codeWrongPassword:
		return fmt.Errorf("proton: %s: wrong password or two-factor code, or the login is locked for a while (code %d)", step, se.Code)
	}
	return fmt.Errorf("proton: %s: %w", step, err)
}

// afterLogin sends what the client sends after a login, in its order: in
// parallel the account, a session certificate, the location and the client
// config, each started once the TCP connection of the one before is open;
// then the feature flags, the server list and the notifications.
// The answers are dropped and errors ignored. The certificate is for a new
// one-off key that is dropped at once: it is not stored, published, counted
// or revoked, and expires by itself.
func (p *Proton) afterLogin(ctx context.Context, s provider.Store) {
	get := func(path string, dialed func(), h ...transport.Header) (transport.Response, error) {
		return p.authed(ctx, s, call{method: http.MethodGet, path: path, header: h, dialed: dialed})
	}
	var netzone string
	var wg sync.WaitGroup
	// start runs f concurrently and returns when f's connection is open or f
	// is done: a hanging dial delays the next start by up to api.timeoutMs
	// per attempt, acceptable for an interactive login.
	start := func(f func(dialed func())) {
		opened := make(chan struct{})
		dialed := sync.OnceFunc(func() { close(opened) })
		wg.Go(func() { defer dialed(); f(dialed) })
		<-opened
	}
	start(func(d func()) { _, _ = get("/vpn/v2", d) })
	start(func(d func()) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return
		}
		_, _ = p.authed(ctx, s, call{method: http.MethodPost, path: "/vpn/v1/certificate", body: obj{
			{"ClientPublicKey", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))},
			{"Duration", "10080 min"},
		}, dialed: d})
	})
	start(func(d func()) {
		resp, err := get("/vpn/v1/location", d)
		var loc struct{ IP string }
		if err != nil || json.Unmarshal(resp.Body, &loc) != nil {
			return
		}
		netzone = zone(loc.IP)
	})
	start(func(d func()) { _, _ = get("/vpn/v2/clientconfig", d) })
	wg.Wait()
	_, _ = get("/feature/v2/frontend", nil)
	h := []transport.Header{{Name: "If-Modified-Since", Value: "Thu, 01 Jan 1970 00:00:00 GMT"}}
	if netzone != "" {
		h = append([]transport.Header{{Name: "X-PM-netzone", Value: netzone}}, h...)
	}
	_, _ = get("/vpn/v1/logicals?SecureCoreFilter=all&WithState=true", nil, h...)
	_, _ = get("/core/v4/notifications", nil)
}

// zone is an IPv4 address with its last octet zeroed, "" for anything else.
func zone(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return ""
	}
	b := a.As4()
	b[3] = 0
	return netip.AddrFrom4(b).String()
}

// Logout revokes the persistent certificates of the provider and forgets
// their keys, closes the session (a 401 means it is already closed) and
// removes it from the state record. Without a session there is nothing to
// close.
func (p *Proton) Logout(ctx context.Context, s provider.Store) error {
	if _, err := p.session(s); err != nil {
		if errors.Is(err, errNeedsLogin) {
			if rec, lerr := loadRecord(s); lerr == nil && rec[sessionKey] == nil {
				return nil
			}
			return errors.New("proton: the session is rejected: sign in again, then log out to revoke the certificates")
		}
		return err
	}
	if err := p.revokeIssued(ctx, s); err != nil {
		return err
	}
	tk, err := p.session(s) // revoking may have refreshed it
	if err != nil {
		return err
	}
	resp, err := p.send(ctx, call{method: http.MethodDelete, path: "/auth", uid: tk.UID, token: tk.AccessToken})
	if err != nil && resp.Status != http.StatusUnauthorized {
		return fmt.Errorf("proton: logout: %w", err)
	}
	if err := saveSession(s, nil); err != nil {
		return err
	}
	p.setSession(nil)
	return nil
}

// revokeIssued revokes every persistent certificate whose DeviceName is
// exactly <prefix><output id>-<gen>-<n> of this provider's output, then
// drops the keys from the state record: their certificates are gone. A
// provider without an output has issued nothing.
func (p *Proton) revokeIssued(ctx context.Context, s provider.Store) error {
	if p.out.ID == "" {
		return nil
	}
	if _, _, err := p.revoke(ctx, s, func(int, string) bool { return true }); err != nil {
		return fmt.Errorf("proton: logout: %w", err)
	}
	return saveMember(s, keysKey, nil)
}

// readLine prompts and reads one line. A secret is read without echo from a
// terminal; other input (a pipe) is read as a line.
func readLine(tty provider.TTY, in *bufio.Reader, label string, secret bool) ([]byte, error) {
	_, _ = fmt.Fprint(tty.Out, label)
	name := strings.ToLower(strings.TrimSuffix(label, ": "))
	var line []byte
	var err error
	if f, ok := tty.In.(*os.File); ok && secret && term.IsTerminal(int(f.Fd())) {
		line, err = term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(tty.Out)
	} else {
		line, err = in.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) > 0 {
			err = nil
		}
		line = bytes.TrimRight(line, "\r\n")
	}
	if err != nil {
		clear(line)
		return nil, fmt.Errorf("read the %s: %w", name, err)
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("empty %s", name)
	}
	return line, nil
}
