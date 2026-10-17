package cli_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/gtl/cli"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func writeConfig(t *testing.T, addr string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	body := "server:\n  listen: " + addr + "\nload:\n  path: /load\n  maxSizeKb: 1048576\nlogging:\n  level: info\n  format: json\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// syncBuf is a concurrency-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	for range 100 {
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

func startServe(ctx context.Context, t *testing.T, addr string) (done chan error, logs *syncBuf) {
	t.Helper()
	logs = &syncBuf{}
	done = make(chan error, 1)
	cfg := writeConfig(t, addr)
	go func() { done <- cli.RunServe(ctx, cfg, logs) }()
	return done, logs
}

func TestServeServesAndStops(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, logs := startServe(ctx, t, addr)

	resp := get(t, "http://"+addr+"/healthz")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServe did not return")
	}
	out := logs.String()
	last := -1
	for _, ev := range []string{`"event":"gtl_server_started"`, `"event":"gtl_server_stopping"`, `"event":"gtl_server_stopped"`} {
		i := strings.Index(out, ev)
		if i <= last {
			t.Fatalf("event %s missing or out of order:\n%s", ev, out)
		}
		last = i
	}
	if !strings.Contains(out, `"listen":"`+addr+`"`) || !strings.Contains(out, `"signal":"context canceled"`) {
		t.Fatalf("log attrs missing:\n%s", out)
	}
}

func TestServeStopsOnSignal(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
	}{{syscall.SIGINT, "SIGINT"}, {syscall.SIGTERM, "SIGTERM"}} {
		t.Run(tc.name, func(t *testing.T) {
			addr := freeAddr(t)
			ctx, stop := cli.NotifyContext(t.Context())
			defer stop()
			done, logs := startServe(ctx, t, addr)
			_ = get(t, "http://"+addr+"/healthz").Body.Close()
			if err := syscall.Kill(syscall.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), `"signal":"`+tc.name+`"`) {
				t.Fatalf("signal name missing:\n%s", logs.String())
			}
		})
	}
}

func TestServeShutdownTimeout(t *testing.T) {
	defer cli.SetShutdownTimeout(200 * time.Millisecond)()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, _ := startServe(ctx, t, addr)
	_ = get(t, "http://"+addr+"/healthz").Body.Close()

	// A client that requests 1 GiB and never reads stalls the response.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /load/1048576 HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "shutdown") {
			t.Fatalf("err = %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("took %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServe did not return")
	}
}

func TestServeBindError(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	logs := &syncBuf{}
	err = cli.RunServe(t.Context(), writeConfig(t, ln.Addr().String()), logs)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("err = %v", err)
	}
	if logs.String() != "" {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

func TestServeFinishesActiveDownload(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, _ := startServe(ctx, t, addr)

	resp := get(t, "http://"+addr+"/load/10240")
	defer resp.Body.Close()
	first := make([]byte, 1024)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	cancel()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(first) + len(rest); n != 10485760 {
		t.Fatalf("received %d bytes", n)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
