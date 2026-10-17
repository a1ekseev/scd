//go:build xraylive

package livetest

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

const (
	root          = "../../../"
	corpusDir     = root + "testdata/reference/scd/corpus/"
	templatesDir  = "testdata/xray/"
	readyTimeout  = 10 * time.Second
	rpcTimeout    = 5 * time.Second
	envXrayBinary = "SCD_XRAY_BIN"
)

// syncBuffer is a bytes.Buffer safe for the Xray output copier and the test
// reading it concurrently (a plain *bytes.Buffer would race under -race).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything Xray has written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// xrayProc is a running Xray started by launchXray.
type xrayProc struct {
	client    xrayapi.Client
	socksAddr string
	log       *syncBuffer
	done      chan struct{} // closed when the process exits
	waitErr   error         // valid after done is closed
}

// exited reports whether the process has already exited.
func (p *xrayProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// startXray starts Xray (SCD_XRAY_BIN) on the testdata/xray templates with
// free ports and the extra environment env, waits until ListOutbounds
// answers and returns the API client, the SOCKS inbound address and Xray's
// combined stdout/stderr. Cleanup fails the test if Xray exited early or
// panicked, then kills it. Without SCD_XRAY_BIN the test is skipped.
func startXray(t *testing.T, env ...string) (client xrayapi.Client, socksAddr string, stderr *syncBuffer) {
	t.Helper()
	p := launchXray(t, env...)
	return p.client, p.socksAddr, p.log
}

func launchXray(t *testing.T, env ...string) *xrayProc {
	t.Helper()
	bin := os.Getenv(envXrayBinary)
	if bin == "" {
		t.Skip(envXrayBinary + " not set")
	}

	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	probePort := probe.Listener.Addr().(*net.TCPAddr).Port
	apiPort, socksPort := freePort(t), freePort(t)

	dir := t.TempDir()
	repl := strings.NewReplacer(
		"{{API_PORT}}", strconv.Itoa(apiPort),
		"{{SOCKS_PORT}}", strconv.Itoa(socksPort),
		"{{PROBE_PORT}}", strconv.Itoa(probePort),
	)
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out := repl.Replace(string(b))
		if !json.Valid([]byte(out)) {
			t.Fatalf("%s is not valid JSON after substitution", e.Name())
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := &xrayProc{
		socksAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)),
		log:       &syncBuffer{},
		done:      make(chan struct{}),
	}
	// The process must outlive the test's context: it is killed in Cleanup.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin, "run", "-confdir", dir)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = p.log, p.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start xray: %v", err)
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		if p.exited() {
			t.Errorf("xray exited before the end of the test: %v\n%s", p.waitErr, p.log.String())
		} else {
			_ = cmd.Process.Kill()
			<-p.done
		}
		if strings.Contains(p.log.String(), "panic:") {
			t.Errorf("xray panicked:\n%s", p.log.String())
		}
	})

	apiAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort))
	p.client, err = xrayapi.Dial(apiAddr, rpcTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.client.Close() })

	deadline := time.Now().Add(readyTimeout)
	for {
		_, err := p.client.ListOutbounds(t.Context())
		if err == nil {
			break
		}
		if p.exited() {
			t.Fatalf("xray exited during startup: %v\n%s", p.waitErr, p.log.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("xray API at %s not ready after %s: %v\n%s", apiAddr, readyTimeout, err, p.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return p
}

// freePort returns a TCP port on 127.0.0.1 that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// d26Case is one line of d26-links.txt with its d26-expect.json row.
type d26Case struct {
	Line           int      `json:"line"`
	MatrixRow      string   `json:"matrixRow"`
	Expect         string   `json:"expect"`
	Stage          string   `json:"stage"`
	ReasonCode     string   `json:"reasonCode"`
	ExpectContains string   `json:"expectContains"`
	Profile        string   `json:"profile"`
	Warnings       []string `json:"warnings"`
	Params         []string `json:"params"`
	Note           string   `json:"note"`
	// Raw is the link line itself.
	Raw string `json:"-"`
}

// loadD26 reads the D26 corpus: d26-links.txt joined with d26-expect.json.
func loadD26(t *testing.T) []d26Case {
	t.Helper()
	data, err := os.ReadFile(corpusDir + "d26-expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []d26Case
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	lines := fileLines(t, corpusDir+"d26-links.txt")
	if len(lines) != len(cases) {
		t.Fatalf("d26-links.txt has %d lines, d26-expect.json %d rows", len(lines), len(cases))
	}
	for i := range cases {
		if cases[i].Line != i+1 {
			t.Fatalf("d26-expect.json row %d has line %d", i+1, cases[i].Line)
		}
		cases[i].Raw = lines[i]
	}
	return cases
}

// fileLines returns the lines of a file (index 0 = line 1).
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// subscriptionLines reads a subscription fixture (plain or base64) as scd does.
func subscriptionLines(t *testing.T, path string) []subscription.Line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := inputsrc.DecodeAuto(string(data))
	lines := subscription.ScanLines(content)
	if len(lines) == 0 {
		t.Fatalf("%s has no lines", path)
	}
	return lines
}
