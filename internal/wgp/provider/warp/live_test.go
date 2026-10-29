//go:build warplive

package warp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// TestLiveMasque registers a new temporary device with the real WARP API,
// switches it to MASQUE, checks it and deletes it. Keys and tokens stay in
// memory and are never logged.
func TestLiveMasque(t *testing.T) {
	if os.Getenv("SCD_WARP_LIVE") != "1" {
		t.Skip("SCD_WARP_LIVE=1 not set")
	}
	api := provider.API{TimeoutMs: 20000}
	api.Retry.Attempts = 1 // no retry series against the real API
	w := &WARP{
		id: "warp", out: provider.Output{ID: "warp", Candidates: 1}, ident: testID,
		cfg: transport.Config{API: api}, now: time.Now,
		Tunnel: tunnelMasque, Label: "WARP", DeviceLostAfterChecks: 2, ReRegisterMinIntervalMinutes: 60,
		Masque: Masque{Transports: []string{"h3", "h2"}, SNI: "consumer-masque.cloudflareclient.com", Port: 443, DNS: cloudflareDNS},
	}
	s := &memStore{}
	dev := func() device {
		var rec record
		if s.b == nil || json.Unmarshal(s.b, &rec) != nil || len(rec.Devices) == 0 {
			return device{}
		}
		return rec.Devices[0]
	}
	t.Cleanup(func() {
		d := dev()
		if d.ID == "" {
			return
		}
		ctx := context.Background()
		if _, err := w.api.do(ctx, http.MethodDelete, "/reg/"+d.ID, d.Token, nil); err != nil {
			t.Errorf("DELETE /reg: %v", err)
			return
		}
		_, err := w.call(ctx, http.MethodGet, "/reg/"+d.ID, d.Token, nil)
		if se, ok := errors.AsType[*transport.StatusError](err); !ok || se.Status != http.StatusNotFound && se.Status != http.StatusUnauthorized {
			t.Errorf("GET after DELETE: %v, want 404 or 401", err)
		}
	})

	res := tick(t, w, s) // POST /reg, then PATCH to MASQUE
	if res.Status.Error != "" || len(res.Lines) != 2 {
		t.Fatalf("tick: %d lines, error %q", len(res.Lines), res.Status.Error)
	}
	parseMasque(t, res.Lines)
	d := dev()
	r, err := w.call(t.Context(), http.MethodGet, "/reg/"+d.ID, d.Token, nil)
	if err != nil {
		t.Fatalf("GET /reg: %v", err)
	}
	if r.Key != w.deviceKey(d) {
		t.Error("GET /reg: the key is not the device MASQUE key")
	}
	if r.Enabled == nil || !*r.Enabled {
		t.Error("GET /reg: enabled is not true")
	}
}
