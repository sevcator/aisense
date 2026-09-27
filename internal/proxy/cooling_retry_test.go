package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

// coolUpstream puts the upstream's key on a cooldown of the given kind.
func coolUpstream(t *testing.T, p *Proxy, key, category string, left time.Duration) {
	t.Helper()
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Upstreams[0].HitchanceState = map[string]hitchance.State{
			config.HitchanceTarget("key", key, "model"): {
				Category: category, RuleID: category, Action: "demote",
				Until: time.Now().Add(left), UpdatedAt: time.Now(), Failures: 1,
			},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func setCoolingRetry(t *testing.T, p *Proxy, seconds int) {
	t.Helper()
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.CoolingRetrySeconds = seconds
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A cooldown that ends in a moment must not turn into "unavailable": the
// request waits for it, the way a client would retry.
func TestRequestWaitsForACooldownThatEndsSoon(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 10)
	// A limit cooldown is never probed, so only the wait can rescue this one.
	coolUpstream(t, p, "a", "limit", 700*time.Millisecond)

	start := time.Now()
	w := hitchanceRequest(p)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if waited := time.Since(start); waited < 600*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("waited %s", waited)
	}
}

// A long cooldown cannot be waited out, but a cooldown is only a guess: the
// upstream is asked once rather than answering "unavailable" untried.
func TestLongCooldownIsProbedOnceInsteadOfFailing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 10)
	coolUpstream(t, p, "a", "outage", 30*time.Minute)

	start := time.Now()
	if w := hitchanceRequest(p); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("probe did not answer: status=%d calls=%d", w.Code, calls.Load())
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("probing took %s", waited)
	}
}

// The probe is throttled: a second request inside the interval does not ask
// the cooling upstream again.
func TestProbeIsNotRepeatedOnEveryRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"still broken"}}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 1)
	coolUpstream(t, p, "a", "outage", 30*time.Minute)

	if w := hitchanceRequest(p); w.Code == 200 {
		t.Fatal("a broken upstream must not answer 200")
	}
	first := calls.Load()
	if first != 1 {
		t.Fatalf("probe calls=%d", first)
	}
	if w := hitchanceRequest(p); w.Code == 200 || calls.Load() != first {
		t.Fatalf("second request probed again: status=%d calls=%d", w.Code, calls.Load())
	}
}

// Zero seconds keeps the old behaviour: answer at once, ask nobody.
func TestCoolingRetryCanBeTurnedOff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 0)
	coolUpstream(t, p, "a", "outage", 30*time.Minute)

	start := time.Now()
	if w := hitchanceRequest(p); w.Code != 503 || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s with the retry off", waited)
	}
}

// A client that gives up must not leave the request waiting on a cooldown.
func TestWaitingStopsWhenTheClientCancels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 60)
	if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.RetryCycles = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	coolUpstream(t, p, "a", "limit", 30*time.Second)

	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	r.Header.Set("Authorization", "Bearer gateway-test-key")
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	p.ServeOpenAI(httptest.NewRecorder(), r)
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("cancelled request waited %s", waited)
	}
}

// The client hears only that the model is unavailable; why stays in the server log.
func TestUnavailableAnswerIsPlain(t *testing.T) {
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"upstream is down"}}`))
	}))
	defer broke.Close()
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"your key is invalid"}}`))
	}))
	defer rejected.Close()

	p := hitchanceProxy(t, broke.URL, []string{"a"})
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.InvalidConfirmations = 5 // do not delete the key in this test
		c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "second", BaseURL: rejected.URL, Enabled: true, AuthMode: "swap", Models: []string{"model"}, APIKeys: []string{"b"}, Priority: 10})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := hitchanceRequest(p)
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	want := `{"error":{"message":"the requested model model is unavailable","type":"unavailable"}}`
	if body := strings.TrimSpace(w.Body.String()); body != want {
		t.Fatalf("answer = %s, want %s", body, want)
	}
}

// A key that hits a rate limit during the request is waited for, instead of the
// model running dry: the request comes back once the limit has passed.
func TestRequestWaitsOutARateLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"TPM limit reached"}}`))
			return
		}
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 10)
	if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.RetryCycles = 0; return nil }); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	w := hitchanceRequest(p)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if waited := time.Since(start); waited < 900*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("waited %s, want about the 1s the provider asked for", waited)
	}
}

func TestUnlimitedRetryWaitsForAnOutageToRecover(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"endpoint is down"}}`))
			return
		}
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 0
		c.Hitchance.CoolingRetrySeconds = 0
		c.Hitchance.Rules = []hitchance.Rule{{
			ID: "endpoint-down", StatusCodes: []int{http.StatusServiceUnavailable},
			Category: "outage", Action: "demote", Scope: "endpoint", CooldownSeconds: 1,
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	w := hitchanceRequest(p)
	if w.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if waited := time.Since(start); waited < 900*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("waited %s for the outage cooldown", waited)
	}
}

// After real attempts only a rate limit is waited for: a rejected key or a down
// API base will not come back by itself, so the answer is immediate.
func TestNoWaitAfterFailuresThatAreNotRateLimits(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"upstream is down"}}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	setCoolingRetry(t, p, 60)

	start := time.Now()
	w := hitchanceRequest(p)
	if w.Code != 503 || time.Since(start) > 3*time.Second {
		t.Fatalf("status=%d after %s", w.Code, time.Since(start))
	}
}
