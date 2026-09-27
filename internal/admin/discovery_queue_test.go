package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
)

func TestDiscoveryParallelProgressAndLimit(t *testing.T) {
	var active, peak atomic.Int32
	gate := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(404)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if r.URL.Path == "/0/models" {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, `{"data":[{"id":"catalog-model"}]}`)
	}))
	defer api.Close()
	defer release.Do(func() { close(gate) })
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		for i := 0; i < 900; i++ {
			c.Upstreams = append(c.Upstreams, &config.Upstream{ID: fmt.Sprint(i), BaseURL: fmt.Sprintf("%s/%d", api.URL, i), Enabled: true, Models: []string{"*"}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.refreshUpstreamModels(context.Background(), nil); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	progress := false
	for time.Now().Before(deadline) {
		for _, u := range cfg.Get().Upstreams {
			if u.ID != "0" && !u.ModelsRefreshedAt.IsZero() {
				progress = true
				break
			}
		}
		if progress {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	release.Do(func() { close(gate) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !progress {
		t.Fatal("slow endpoint blocked progressive persistence")
	}
	if peak.Load() < 2 || peak.Load() > 8 {
		t.Fatalf("concurrency=%d", peak.Load())
	}
	for _, u := range cfg.Get().Upstreams {
		if strings.Join(u.Models, ",") != "catalog-model" {
			t.Fatal("catalog missing")
		}
	}
}

func TestImportDiscoveryRunsWithPeriodicOffAndRequestCancelled(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("URL-only import sent credentials")
		}
		if r.Method != "GET" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("root endpoint=%s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"root-model"}]}`)
	}))
	defer api.Close()
	s, cfg := testServer(t)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	s.importUpstream(w, httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"text":%q}`, api.URL))).WithContext(requestCtx))
	cancelRequest()
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"models_discovery_queued":true`) {
		t.Fatal("not queued")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartModelDiscovery(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Join(cfg.Get().Upstreams[0].Models, ",") == "root-model" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("periodic-off import did not discover")
}

func TestDiscoveryRejectsStaleCredentialsAndPreservesFailureCatalog(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "u", BaseURL: "http://example.invalid", Enabled: true, APIKeys: []string{"old"}, Models: []string{"kept"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := cfg.Get()
	if err := cfg.Update(func(c *config.Config) error { c.Upstreams[0].APIKeys = []string{"new"}; return nil }); err != nil {
		t.Fatal(err)
	}
	result := map[string]modelRefreshResult{"u": {models: []string{"stale"}, modelAliases: map[string][]string{"stale": {"stale"}}, selectedBase: "http://example.invalid"}}
	if n, err := s.commitModelResults(snapshot, false, result); err != nil || n != 0 {
		t.Fatalf("stale apply: %d %v", n, err)
	}
	result = map[string]modelRefreshResult{"u": {err: fmt.Errorf("HTTP 401")}}
	if _, err := s.commitModelResults(cfg.Get(), false, result); err != nil {
		t.Fatal(err)
	}
	u := cfg.Get().Upstreams[0]
	if strings.Join(u.Models, ",") != "kept" || u.ModelsRefreshError != "HTTP 401" {
		t.Fatal("failure lost catalog or error")
	}
}

func TestDiscoveryDeadlineCoversAllKeysAndSchemes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := discoverUpstreamModels(ctx, &config.Upstream{BaseURL: api.URL, APIKeys: []string{"one", "two", "three"}}, config.ModelDiscoveryCfg{AutoFixProblems: true}, nil)
	if r.err == nil || time.Since(start) > time.Second {
		t.Fatal("endpoint budget multiplied across keys/schemes")
	}
}

func TestDiscoveryQueueCoalescesAndDoesNotOverlap(t *testing.T) {
	var calls, active, peak atomic.Int32
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(404)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, `{"data":[{"id":"model"}]}`)
	}))
	defer api.Close()
	defer once.Do(func() { close(release) })
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "u", BaseURL: api.URL, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartModelDiscovery(ctx)
	s.queueModelDiscovery([]string{"u"})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("job never started")
	}
	for i := 0; i < 50; i++ {
		s.queueModelDiscovery([]string{"u"})
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.discoveryMu.Lock()
		idle := !s.discoveryStatus.Running && len(s.discoveryPending) == 0
		s.discoveryMu.Unlock()
		if idle && calls.Load() == 2 {
			if peak.Load() != 1 {
				t.Fatal("overlapping refreshes")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue did not coalesce: requests=%d", calls.Load())
}

func TestDiscoveryDoesNotApplyChangedPooledTwinOrTouchDisabled(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.AutoFixProblems = true
		c.Upstreams = []*config.Upstream{
			{ID: "http", Type: "openai", BaseURL: "http://example.invalid/v1", Enabled: true, APIKeys: []string{"a"}, Models: []string{"kept"}},
			{ID: "https", Type: "openai", BaseURL: "https://example.invalid/v1", Enabled: true, APIKeys: []string{"b"}, Models: []string{"kept"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := cfg.Get()
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams[1].APIKeys = []string{"changed"}
		c.Upstreams[1].Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	results := map[string]modelRefreshResult{}
	for _, id := range []string{"http", "https"} {
		results[id] = modelRefreshResult{models: []string{"stale"}, modelAliases: map[string][]string{"stale": {"stale"}}, selectedBase: "https://example.invalid/v1"}
	}
	if n, err := s.commitModelResults(snapshot, false, results); err != nil || n != 0 {
		t.Fatalf("pooled stale apply: %d %v", n, err)
	}
	if len(cfg.Get().Upstreams) != 2 {
		t.Fatal("edited twin merged")
	}
	for _, u := range cfg.Get().Upstreams {
		if strings.Join(u.Models, ",") != "kept" || !u.ModelsRefreshedAt.IsZero() {
			t.Fatal("stale catalog applied")
		}
	}
}
