package inputsrc

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFilePlain(t *testing.T) {
	p := writeFile(t, "vless://a\nvless://b\n")
	got, err := Load(context.Background(), p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != p || got.Encoding != "plain" || got.Content != "vless://a\nvless://b\n" {
		t.Fatalf("got %#v", got)
	}
}

func TestLoadFileWithBOM(t *testing.T) {
	p := writeFile(t, "\xEF\xBB\xBFvless://a\n")
	got, err := Load(context.Background(), p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Content, "vless://") {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestLoadFileIgnoresFetchTimeout(t *testing.T) {
	p := writeFile(t, "vless://a\n")
	if _, err := Load(context.Background(), p, Options{FetchTimeout: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLoadHTTPBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(b64("vless://a\nvless://b")))
	}))
	defer srv.Close()
	got, err := Load(context.Background(), srv.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != srv.URL || got.Encoding != "base64" || got.Content != "vless://a\nvless://b" {
		t.Fatalf("got %#v", got)
	}
}

func TestLoadHTTPUserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("vless://a"))
	}))
	defer srv.Close()
	if _, err := Load(context.Background(), srv.URL, Options{}); err != nil {
		t.Fatal(err)
	}
	if ua != "scd/dev" {
		t.Fatalf("default UA = %q", ua)
	}
	if _, err := Load(context.Background(), srv.URL, Options{UserAgent: "Happ/1.0"}); err != nil {
		t.Fatal(err)
	}
	if ua != "Happ/1.0" {
		t.Fatalf("UA = %q", ua)
	}
	// Headers cannot override the user agent.
	if _, err := Load(context.Background(), srv.URL, Options{UserAgent: "Happ/1.0", Headers: Headers{"User-Agent": "evil"}}); err != nil {
		t.Fatal(err)
	}
	if ua != "Happ/1.0" {
		t.Fatalf("UA = %q", ua)
	}
}

func TestLoadHTTPSendsFetchHeaders(t *testing.T) {
	var hwid, osName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hwid, osName = r.Header.Get("X-Hwid"), r.Header.Get("X-Device-Os")
		_, _ = w.Write([]byte("vless://a"))
	}))
	defer srv.Close()
	h := Headers{"x-hwid": "abc", "X-Device-OS": "linux"}
	if _, err := Load(context.Background(), srv.URL, Options{Headers: h}); err != nil {
		t.Fatal(err)
	}
	if hwid != "abc" || osName != "linux" {
		t.Fatalf("headers = %q %q", hwid, osName)
	}

	// File and stdin ignore headers.
	if _, err := Load(context.Background(), writeFile(t, "vless://a"), Options{Headers: h}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), "-", Options{Headers: h, Stdin: strings.NewReader("vless://a")}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadErrorDoesNotLeakHeaderValues(t *testing.T) {
	h := Headers{"x-hwid": "secret-hwid-value"}
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer forbidden.Close()
	_, err := Load(context.Background(), forbidden.URL, Options{Headers: h})
	if err == nil || strings.Contains(err.Error(), "secret-hwid-value") {
		t.Fatalf("err = %v", err)
	}

	done := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-done }))
	defer hang.Close()
	defer close(done)
	_, err = Load(context.Background(), hang.URL, Options{Headers: h, FetchTimeout: 20 * time.Millisecond})
	if err == nil || strings.Contains(err.Error(), "secret-hwid-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadHTTPTimeout(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-done }))
	defer srv.Close()
	defer close(done)
	_, err := Load(context.Background(), srv.URL, Options{FetchTimeout: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out after 10ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadHTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := Load(context.Background(), srv.URL, Options{})
	want := "unable to download input from " + srv.URL + ": 404 Not Found"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestLoadEmptyRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("  \n"))
	}))
	defer srv.Close()
	_, err := Load(context.Background(), srv.URL, Options{})
	if !errors.Is(err, ErrEmpty) || err.Error() != "input source "+srv.URL+" returned empty content" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	_, err := Load(context.Background(), writeFile(t, "   \n"), Options{})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadStdinBase64(t *testing.T) {
	got, err := Load(context.Background(), "-", Options{Stdin: strings.NewReader(b64("vless://a") + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "stdin" || got.Encoding != "base64" || got.Content != "vless://a" {
		t.Fatalf("got %#v", got)
	}
}

func TestLoadEmptyStdin(t *testing.T) {
	_, err := Load(context.Background(), "-", Options{Stdin: strings.NewReader("")})
	if !errors.Is(err, ErrEmpty) || err.Error() != "input source stdin returned empty content" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadEmptyAfterDecode(t *testing.T) {
	// base64 of whitespace-only text without "://" stays as is (plain), so use forced base64.
	_, err := Load(context.Background(), "-", Options{Format: "base64", Stdin: strings.NewReader(b64("  \n"))})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadFormats(t *testing.T) {
	got, err := Load(context.Background(), "-", Options{Format: "plain", Stdin: strings.NewReader(b64("vless://a"))})
	if err != nil || got.Encoding != "plain" || got.Content != b64("vless://a") {
		t.Fatalf("plain: %#v %v", got, err)
	}
	got, err = Load(context.Background(), "-", Options{Format: "base64", Stdin: strings.NewReader(b64("vless://a"))})
	if err != nil || got.Encoding != "base64" || got.Content != "vless://a" {
		t.Fatalf("base64: %#v %v", got, err)
	}
}

func TestLoadBase64FormatInvalid(t *testing.T) {
	_, err := Load(context.Background(), "-", Options{Format: "base64", Stdin: strings.NewReader("not base64 !!")})
	if err == nil || !strings.Contains(err.Error(), "is not valid base64") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeAuto(t *testing.T) {
	const plain = "vless://uuid@host:443?type=tcp#name\nvless://b@h:1#x"
	std := base64.StdEncoding.EncodeToString([]byte(plain))
	var wrapped strings.Builder
	for i := 0; i < len(std); i += 20 {
		wrapped.WriteString(std[i:min(i+20, len(std))] + "\n")
	}
	tests := []struct {
		name, in, want string
		decoded        bool
	}{
		{"std", std, plain, true},
		{"url-safe", base64.URLEncoding.EncodeToString([]byte("vless://a?b>>>")), "vless://a?b>>>", true},
		{"no padding", base64.RawStdEncoding.EncodeToString([]byte("vless://ab")), "vless://ab", true},
		{"wrapped", wrapped.String(), plain, true},
		{"surrounding spaces", "  " + std + " \n", plain, true},
		{"hello world", "hello world", "hello world", false},
		{"plain vless", "vless://a@b:1#x", "vless://a@b:1#x", false},
		{"base64 without scheme", b64("hello world"), b64("hello world"), false},
		{"invalid char", "dmxl!c3M6Ly9h", "dmxl!c3M6Ly9h", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, dec := DecodeAuto(tc.in)
			if got != tc.want || dec != tc.decoded {
				t.Fatalf("DecodeAuto = %q, %v; want %q, %v", got, dec, tc.want, tc.decoded)
			}
		})
	}
}

func TestLoadRedirectHeaders(t *testing.T) {
	var gotOther, gotSame string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOther = r.Header.Get("X-Hwid")
		_, _ = w.Write([]byte("vless://a"))
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/cross", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		gotSame = r.Header.Get("X-Hwid")
		_, _ = w.Write([]byte("vless://a"))
	})
	first := httptest.NewServer(mux)
	defer first.Close()
	h := Headers{"x-hwid": "secret"}

	client := &http.Client{}
	if _, err := Load(context.Background(), first.URL+"/cross", Options{Headers: h, HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	if gotOther != "" {
		t.Fatalf("header leaked to other host: %q", gotOther)
	}
	if client.CheckRedirect != nil {
		t.Fatal("caller client was mutated")
	}
	if _, err := Load(context.Background(), first.URL+"/same", Options{Headers: h}); err != nil {
		t.Fatal(err)
	}
	if gotSame != "secret" {
		t.Fatalf("same-host header = %q", gotSame)
	}
}

func TestLoadRedirectChainsCallerCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
	}))
	defer srv.Close()
	_, err := Load(context.Background(), srv.URL, Options{}) // default limit
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	called := false
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { called = true; return http.ErrUseLastResponse }}
	_, _ = Load(context.Background(), srv.URL, Options{HTTPClient: c})
	if !called {
		t.Fatal("caller CheckRedirect not chained")
	}
}

func TestLoadBodyTooLarge(t *testing.T) {
	old := maxBody
	maxBody = 8
	defer func() { maxBody = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("vless://aaaaaaaa"))
	}))
	defer srv.Close()
	_, err := Load(context.Background(), srv.URL, Options{})
	if err == nil || !strings.Contains(err.Error(), "input from "+srv.URL+" exceeds") {
		t.Fatalf("err = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Deferred 1.6: a same-host redirect from https to http drops fetchHeaders
// (the secret would travel in clear text).
func TestLoadRedirectDowngradeStripsHeaders(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/final", http.StatusFound)
	}))
	defer srv.Close()
	client := srv.Client()
	base := client.Transport
	var (
		seen bool
		got  string
	)
	// Plain-http requests are answered here: the TLS server's port cannot
	// speak http, and the test only needs the headers of that request.
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "http" {
			return base.RoundTrip(r)
		}
		seen, got = true, r.Header.Get("X-Hwid")
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("vless://a")),
		}, nil
	})
	if _, err := Load(context.Background(), srv.URL+"/sub", Options{Headers: Headers{"x-hwid": "secret"}, HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the http redirect target was not requested")
	}
	if got != "" {
		t.Fatalf("header sent over the downgraded redirect: %q", got)
	}
}
