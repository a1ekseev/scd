package static

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/config"
)

const (
	priv = "MTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTE="
	pub  = "MjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjI="
)

// valid is a minimal accepted file; tests change parts of it.
const valid = `[Interface]
PrivateKey = ` + priv + `
Address = 10.9.0.2/32

[Peer]
PublicKey = ` + pub + `
Endpoint = 192.0.2.31:51820
`

func newTest(dir string) *Static {
	return &Static{id: "s", Dir: dir, MaxFileBytes: 1024, Defaults: Defaults{MTU: 1420, AllowedIPs: []string{"0.0.0.0/0", "::/0"}}}
}

func TestFile(t *testing.T) {
	base := "wireguard://" + priv + "@192.0.2.31:51820?provider=static&publickey=" + pub + "&address=10.9.0.2/32"
	cases := []struct {
		name, body string
		mode       os.FileMode
		want       string // the published line, or
		wantErr    string // the error
		warn       string
	}{
		{name: "valid with defaults", body: valid, want: base + "&mtu=1420&allowedips=0.0.0.0/0,::/0#a"},
		{
			name: "all fields",
			body: "# Label = 🇳🇱 Amsterdam\n# reserved = 1, 2,3\n" + strings.Replace(strings.Replace(valid,
				"Address = 10.9.0.2/32\n", "address = 10.9.0.2/32, fd09::2/128\nDNS = 192.0.2.53\nDNS = 192.0.2.54 # second\nMTU = 1380\n", 1),
				"Endpoint", "PresharedKey = "+priv+"\nAllowedIPs = 0.0.0.0/0\nPersistentKeepalive = 25\nEndpoint", 1),
			want: "wireguard://" + priv + "@192.0.2.31:51820?provider=static&publickey=" + pub +
				"&address=10.9.0.2/32,fd09::2/128&dns=192.0.2.53,192.0.2.54&mtu=1380&allowedips=0.0.0.0/0&reserved=1,2,3&keepalive=25&presharedkey=" + priv +
				"#%F0%9F%87%B3%F0%9F%87%B1%20Amsterdam",
		},
		{name: "keepalive off", body: valid + "PersistentKeepalive = off\n", want: base + "&mtu=1420&allowedips=0.0.0.0/0,::/0#a"},
		{name: "PostUp ignored", body: strings.Replace(valid, "[Peer]", "PostUp = rm -rf /\n[Peer]", 1), want: base + "&mtu=1420&allowedips=0.0.0.0/0,::/0#a", warn: "PostUp ignored"},
		{name: "no peer", body: strings.Split(valid, "[Peer]")[0], wantErr: "exactly one [Peer], has 0"},
		{name: "two peers", body: valid + "[Peer]\nPublicKey = " + pub + "\n", wantErr: "exactly one [Peer], has 2"},
		{name: "domain in DNS", body: strings.Replace(valid, "[Peer]", "DNS = example.com\n[Peer]", 1), wantErr: "candidate field dns: each must be an IP"},
		{name: "too large", body: valid + "#" + strings.Repeat("x", 1024) + "\n", wantErr: "larger than maxFileBytes 1024"},
		{name: "0644", body: valid, mode: 0o644, wantErr: "mode 0644: must have no access for others"},
		{name: "0640 allowed", body: valid, mode: 0o640, want: base + "&mtu=1420&allowedips=0.0.0.0/0,::/0#a"},
		{name: "unknown key", body: valid + "Foo = 1\n", wantErr: "unknown key Foo in [peer]"},
		{name: "ignored key in peer", body: valid + "PostUp = x\n", wantErr: "unknown key PostUp in [peer]"},
		{name: "repeated key", body: valid + "Endpoint = 192.0.2.1:1\n", wantErr: "repeated key Endpoint"},
		{name: "outside section", body: "MTU = 1\n" + valid, wantErr: "outside a section"},
		{name: "unknown section", body: valid + "[Other]\n", wantErr: "unknown section"},
		{name: "secret not quoted", body: strings.Replace(valid, "PrivateKey = ", "PrivateKey ", 1), wantErr: "line 2: not a key = value line"},
		{name: "endpoint injection", body: strings.Replace(valid, "51820", "51820?x=1", 1), wantErr: "endpoint: must be host:port"},
		{name: "bad key", body: strings.Replace(valid, pub, "short", 1), wantErr: "candidate field publickey"},
		{name: "no public key", body: strings.Replace(valid, "PublicKey = "+pub+"\n", "", 1), wantErr: "candidate field publickey: required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "a.conf")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.mode != 0 {
				if err := os.Chmod(p, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			line, warns, err := newTest(dir).file("a.conf")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), priv) || strings.Contains(err.Error(), pub) {
					t.Fatalf("error leaks a key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if line != tc.want {
				t.Fatalf("line\n%s\nwant\n%s", line, tc.want)
			}
			if _, err := candidate.ParseWireGuard(line); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(warns, "\n"); !strings.Contains(got, tc.warn) || (tc.warn == "") != (got == "") {
				t.Fatalf("warnings %q, want %q", got, tc.warn)
			}
		})
	}
}

// A rejected file is reported, the others are published in name order;
// other files are skipped.
func TestTick(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"b.conf":    strings.Replace(valid, "192.0.2.31", "192.0.2.32", 1),
		"a.conf":    valid,
		"bad.conf":  "[Interface]\n",
		"notes.txt": "x",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := newTest(dir).Tick(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 2 || !strings.Contains(res.Lines[0], "#a") || !strings.Contains(res.Lines[1], "#b") {
		t.Fatalf("lines %q", res.Lines)
	}
	if res.Status.State != wgpstatus.StateError || res.Status.Issued != 2 || !strings.HasPrefix(res.Status.Error, "bad.conf: ") {
		t.Fatalf("status %+v", res.Status)
	}

	if err := os.Remove(filepath.Join(dir, "a.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "b.conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := newTest(dir).Tick(t.Context(), nil); err == nil || !strings.HasPrefix(err.Error(), "bad.conf: ") {
		t.Fatalf("err %v", err)
	}
	if _, err := newTest(t.TempDir()).Tick(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "no .conf files") {
		t.Fatalf("err %v", err)
	}
}

// The tracked fixtures are accepted (git keeps them 0644, so copy them 0600).
func TestFixtures(t *testing.T) {
	src := "../../../../docker/fixtures/static"
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := newTest(dir).Tick(t.Context(), nil)
	if err != nil || res.Status.State != wgpstatus.StateOK || len(res.Lines) != len(entries) {
		t.Fatalf("err %v status %+v lines %d", err, res.Status, len(res.Lines))
	}
}

func TestConfig(t *testing.T) {
	const cfg = `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
deviceNamePrefix: wgp-a-
providers:
  - {id: st, type: static, schedule: "@hourly", dir: /app/static%s}
outputs:
  - {id: wg, path: /app/out/wg, provider: st%s}
`
	cases := []struct{ name, provider, output, want string }{
		{name: "defaults"},
		{name: "candidates", output: ", candidates: 1", want: `outputs.0.candidates: not allowed for provider type "static"`},
		{name: "mtu", provider: ", defaults: {mtu: 100}", want: "providers.0.defaults.mtu: must be at least 576"},
		{name: "allowedIPs", provider: ", defaults: {allowedIPs: [10.0.0.1]}", want: "providers.0.defaults: candidate field allowedips"},
		{name: "dns domain", provider: ", defaults: {dns: [example.com]}", want: "providers.0.defaults: candidate field dns"},
		{name: "unknown key", provider: ", extra: 1", want: `providers.0: unknown key "extra"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(p, []byte(fmt.Sprintf(cfg, tc.provider, tc.output)), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := config.Load(p)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			s := l.Config.Providers[0].Impl.(*Static)
			if s.Dir != "/app/static" || s.MaxFileBytes != 64<<10 || s.Defaults.MTU != 1420 || len(s.Defaults.DNS) != 0 ||
				strings.Join(s.Defaults.AllowedIPs, ",") != "0.0.0.0/0,::/0" {
				t.Fatalf("static %+v", s)
			}
		})
	}
}
