// Package inputsrc loads subscription text from a file, an HTTP(S) URL or stdin.
package inputsrc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/neterr"
)

const defaultFetchTimeout = 5 * time.Second

// bom is the UTF-8 byte order mark (U+FEFF).
const bom = "\xEF\xBB\xBF"

var httpRe = regexp.MustCompile(`^(?i)https?://`)

// ErrEmpty is returned (wrapped) when a source has no content.
var ErrEmpty = errors.New("empty content")

// Options configures Load.
type Options struct {
	// Format is auto, plain or base64; "" means auto.
	Format string
	// FetchTimeout limits an HTTP request including the body; 0 means 5s.
	FetchTimeout time.Duration
	// UserAgent overrides the default buildinfo.UserAgent().
	UserAgent string
	// Headers are extra request headers (D33), used for http(s) only.
	Headers Headers
	// Stdin is read for input "-"; nil means os.Stdin.
	Stdin io.Reader
	// HTTPClient overrides http.DefaultClient.
	HTTPClient *http.Client
	// MaxBytes caps the raw input size for every source; 0 means 64 MiB for
	// http(s) and no limit for a file or stdin.
	MaxBytes int64
}

// Loaded is a loaded and decoded subscription source.
type Loaded struct {
	// Source is "stdin", the original URL or an absolute file path.
	Source string
	// Content is the decoded text.
	Content string
	// Encoding is "plain" or "base64" (what the source was).
	Encoding string
}

type emptyError struct{ source string }

func (e emptyError) Error() string { return "input source " + e.source + " returned empty content" }
func (emptyError) Unwrap() error   { return ErrEmpty }

// Load reads input ("-" stdin, http(s) URL, otherwise a file path) and decodes it.
func Load(ctx context.Context, input string, o Options) (Loaded, error) {
	var (
		source, raw string
		err         error
	)
	switch {
	case input == "-":
		source = "stdin"
		raw, err = readStdin(o)
	case httpRe.MatchString(input):
		source = input
		raw, err = fetch(ctx, input, o)
	default:
		source, err = filepath.Abs(input)
		if err == nil {
			raw, err = readFile(source, o.MaxBytes)
		}
	}
	if err != nil {
		return Loaded{}, err
	}

	raw = strings.TrimPrefix(raw, bom)
	if strings.TrimSpace(raw) == "" {
		return Loaded{}, emptyError{source}
	}

	content, encoding := raw, "plain"
	switch o.Format {
	case "", "auto":
		if dec, ok := DecodeAuto(raw); ok {
			content, encoding = dec, "base64"
		}
	case "plain":
	case "base64":
		dec, derr := decodeBase64(raw)
		if derr != nil {
			return Loaded{}, fmt.Errorf("input source %s is not valid base64: %w", source, derr)
		}
		content, encoding = dec, "base64"
	default:
		return Loaded{}, fmt.Errorf("unknown input format %q", o.Format)
	}
	content = strings.TrimPrefix(content, bom)
	if strings.TrimSpace(content) == "" {
		return Loaded{}, emptyError{source}
	}
	return Loaded{Source: source, Content: content, Encoding: encoding}, nil
}

func readStdin(o Options) (string, error) {
	r := o.Stdin
	if r == nil {
		r = os.Stdin
	}
	if o.MaxBytes > 0 {
		r = io.LimitReader(r, o.MaxBytes+1)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("unable to read input from stdin: %w", err)
	}
	if o.MaxBytes > 0 && int64(len(b)) > o.MaxBytes {
		return "", tooLarge("stdin", o.MaxBytes)
	}
	return string(b), nil
}

func readFile(path string, limit int64) (string, error) {
	if limit <= 0 {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", tooLarge(path, limit)
	}
	return string(b), nil
}

// tooLarge reports input over limit, printed in MiB, KiB or bytes, whichever
// divides it exactly.
func tooLarge(source string, limit int64) error {
	size := fmt.Sprintf("%d bytes", limit)
	switch {
	case limit%(1<<20) == 0:
		size = fmt.Sprintf("%d MiB", limit>>20)
	case limit%(1<<10) == 0:
		size = fmt.Sprintf("%d KiB", limit>>10)
	}
	return fmt.Errorf("input from %s exceeds %s", source, size)
}

func fetch(ctx context.Context, rawURL string, o Options) (string, error) {
	timeout := o.FetchTimeout
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fail := func(err error) (string, error) {
		if ctx.Err() == nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("input source %s timed out after %dms", rawURL, timeout.Milliseconds())
		}
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // drop the duplicated URL
		}
		// Without the local address the text is the same on every attempt.
		return "", fmt.Errorf("unable to download input from %s: %w", rawURL, neterr.Stable(err))
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fail(err)
	}
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	ua := o.UserAgent
	if ua == "" {
		ua = buildinfo.UserAgent()
	}
	req.Header.Set("User-Agent", ua)

	resp, err := redirectSafeClient(o).Do(req)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("unable to download input from %s: %s", rawURL, resp.Status)
	}
	limit := o.MaxBytes
	if limit <= 0 {
		limit = maxBody
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fail(err)
	}
	if int64(len(b)) > limit {
		return "", tooLarge(rawURL, limit)
	}
	return string(b), nil
}

// maxBody caps a download when Options.MaxBytes is 0 (a var for tests).
var maxBody int64 = 64 << 20

// redirectSafeClient returns a copy of the caller's client (or the default one)
// that drops fetchHeaders (D33 secrets) from redirects leaving the original
// host or downgrading https to http. The caller's client is not modified.
func redirectSafeClient(o Options) *http.Client {
	base := o.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	c := *base
	prev := base.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		orig := via[0].URL
		if req.URL.Host != orig.Host || (orig.Scheme == "https" && req.URL.Scheme != "https") {
			for k := range o.Headers {
				req.Header.Del(k)
			}
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

// DecodeAuto decodes raw as base64 when it looks like a base64-encoded list of
// URIs (decoded text contains "://"); otherwise raw is returned unchanged.
func DecodeAuto(raw string) (content string, decoded bool) {
	if strings.Contains(strings.TrimSpace(raw), "://") {
		return raw, false
	}
	dec, err := decodeBase64(raw)
	if err != nil || !strings.Contains(dec, "://") {
		return raw, false
	}
	return dec, true
}

// decodeBase64 strips all white space and accepts the standard and URL-safe
// alphabets, with or without padding.
func decodeBase64(raw string) (string, error) {
	s := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, raw)
	var first error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(s)
		if err == nil {
			return string(b), nil
		}
		if first == nil {
			first = err
		}
	}
	return "", first
}
