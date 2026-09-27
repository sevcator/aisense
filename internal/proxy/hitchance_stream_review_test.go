package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

type hitchanceFailWriter struct {
	*httptest.ResponseRecorder
	writes, failAt int
	cancel         context.CancelFunc
}

func (w *hitchanceFailWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	n, err := w.ResponseRecorder.Write(b)
	if w.cancel != nil {
		w.cancel()
	}
	return n, err
}

func TestHitchanceR4IncompleteNativeStreamKeepsConfirmations(t *testing.T) {
	for _, mode := range []string{"read-first", "read-later", "write-first", "write-later", "cancel", "eof"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) > 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(401)
					w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				data := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
				if strings.HasPrefix(mode, "read-") {
					w.Header().Set("Content-Length", "4096")
				}
				if mode == "read-first" {
					w.Write([]byte(strings.TrimSuffix(data, "\n\n")))
					return
				}
				w.Write([]byte(data + data))
			}))
			defer server.Close()
			p := hitchanceProxy(t, server.URL, []string{"test-key"})
			target := config.HitchanceTarget("key", "test-key", "model")
			if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.InvalidConfirmations = 2; return nil }); err != nil {
				t.Fatal(err)
			}
			d := hitchance.Classify(p.Cfg.Get().Hitchance, hitchance.Input{Status: 401, Body: []byte(`{"error":{"message":"invalid api key"}}`)})
			if err := p.Cfg.ObserveHitchance(p.Cfg.Get().Upstreams[0], "test-key", "model", d); err != nil {
				t.Fatal(err)
			}
			if err := p.Cfg.Update(func(c *config.Config) error {
				s := c.Upstreams[0].HitchanceState[target]
				s.Until = time.Now().Add(-time.Second)
				c.Upstreams[0].HitchanceState[target] = s
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := p.Cfg.Get().Upstreams[0].HitchanceState[target]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[],"stream":true}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer gateway-test-key")
			w := &hitchanceFailWriter{ResponseRecorder: httptest.NewRecorder()}
			if mode == "write-first" {
				w.failAt = 1
			}
			if mode == "write-later" {
				w.failAt = 2
			}
			if mode == "cancel" {
				w.cancel = cancel
			}
			p.ServeOpenAI(w, r)
			after, exists := p.Cfg.Get().Upstreams[0].HitchanceState[target]
			if mode == "eof" {
				if exists {
					t.Error("normal EOF did not clear history")
				}
			} else if !exists || after != before {
				t.Errorf("incomplete stream cleared/changed confirmation: before=%+v after=%+v", before, after)
			}
			if calls.Load() != 1 {
				t.Errorf("retried committed response: %d", calls.Load())
			}
			health := p.Health.Snapshot(p.Cfg.Get().Upstreams)[0]
			if (health.LastSuccessAt != nil) != (mode == "eof") {
				t.Errorf("native stream completion misreported to runtime health: %+v", health)
			}
			// A second real rejection must still be the second confirmation.
			hitchanceRequest(p)
			deleted := len(p.Cfg.Get().Upstreams) == 0
			if deleted != (mode != "eof") {
				t.Errorf("second rejection deletion=%v; mode=%s", deleted, mode)
			}
			if calls.Load() != 2 {
				t.Errorf("second rejection did not cross forwarding seam: calls=%d", calls.Load())
			}
		})
	}
}
