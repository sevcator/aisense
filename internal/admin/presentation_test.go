package admin

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

func TestDashboardPresentationParity(t *testing.T) {
	s, cfg := testServer(t)
	names := []string{"0g-deepseek-v4-flash", "deepseek-v4-flash", "orphan-low", "orphan-max", "model-fast", "*", "auto", "abcdefghijklmnopqrst", "abcdefghijklmnopqrsx"}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Models: names}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	routes := []CachedRouteEntry{}
	for _, name := range names {
		routes = append(routes, CachedRouteEntry{UpstreamID: "up", Model: name})
	}
	s.Proxy = &mockProxyInfo{routes: routes}
	for _, fast := range []bool{false, true} {
		if err := cfg.Update(func(c *config.Config) error { c.Models.FastMode = fast; return nil }); err != nil {
			t.Fatal(err)
		}
		want := []string{"abcdefghijklmnopqrst", "deepseek-v4-flash", "orphan-low", "orphan-max", "model-fast"}
		sort.Strings(want)
		for _, path := range []string{"state", "cached-routes"} {
			req := httptest.NewRequest("GET", "/admin/api/"+path, nil)
			req.SetBasicAuth("admin", "correct-horse")
			res := httptest.NewRecorder()
			s.Handle(res, req)
			var data struct {
				Models []string           `json:"presentation_models"`
				Routes []CachedRouteEntry `json:"routes"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if res.Code != 200 || !reflect.DeepEqual(data.Models, want) {
				t.Fatalf("%s fast=%v: %d %s", path, fast, res.Code, res.Body.String())
			}
			if path == "cached-routes" && !reflect.DeepEqual(data.Routes, routes) {
				t.Fatal("raw cached routes changed")
			}
		}
	}
}

func TestVariantSettingsPartialAndDashboardMatrix(t *testing.T) {
	s, cfg := testServer(t)
	names := []string{"deepseek-v4-flash", "0g-deepseek-v4-flash", "deepseek-v4-flash-low", "deepseek-v4-flash-free", "deepseek-v4-flash-fast"}
	routes := []CachedRouteEntry{}
	for _, name := range names {
		routes = append(routes, CachedRouteEntry{UpstreamID: "up", Model: name})
	}
	s.Proxy = &mockProxyInfo{routes: routes}
	for mask := 0; mask < 8; mask++ {
		reasoning := mask&1 != 0
		if err := cfg.Update(func(c *config.Config) error {
			c.Models = config.ModelsCfg{ReasoningVariants: &reasoning, PreferOtherVariants: mask&2 != 0, FastMode: mask&4 != 0}
			c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Models: names}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := modelalias.PresentationNames(names, cfg.Get().Models.VariantOptions())
		for _, path := range []string{"state", "cached-routes"} {
			req := httptest.NewRequest("GET", "/admin/api/"+path, nil)
			req.SetBasicAuth("admin", "correct-horse")
			res := httptest.NewRecorder()
			s.Handle(res, req)
			var data struct {
				Models []string `json:"presentation_models"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if res.Code != 200 || !reflect.DeepEqual(data.Models, want) {
				t.Fatalf("mask %d %s: %s", mask, path, res.Body.String())
			}
		}
		before := cfg.Get().Models.VariantOptions()
		for _, payload := range []string{`{"models":{}}`, `{"usage":{"enabled":true}}`} {
			res := httptest.NewRecorder()
			s.saveSettings(res, httptest.NewRequest("POST", "/", strings.NewReader(payload)))
			if res.Code != 200 || cfg.Get().Models.VariantOptions() != before {
				t.Fatalf("partial save %s: %s", payload, res.Body.String())
			}
		}
		res := httptest.NewRecorder()
		s.saveSettings(res, httptest.NewRequest("POST", "/", strings.NewReader(`{"models":{"fast_mode":true}}`)))
		after := cfg.Get().Models.VariantOptions()
		if res.Code != 200 || !after.Fast || after.Reasoning != before.Reasoning || after.Other != before.Other {
			t.Fatal("fast-only save reset other controls")
		}
	}
}

func TestDisabledHiddenStateCacheAndEditorPreservation(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "on", Enabled: true, Models: []string{"gpt-on"}}, {ID: "off", Enabled: false, Models: []string{"gpt-off"}}, {ID: "hidden", Enabled: true, HiddenInvalid: true, Models: []string{"gpt-hidden"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Proxy = &mockProxyInfo{used: []string{"gpt-on", "gpt-off", "gpt-hidden"}, routes: []CachedRouteEntry{{UpstreamID: "on", Model: "gpt-on"}, {UpstreamID: "off", Model: "gpt-off"}, {UpstreamID: "hidden", Model: "gpt-hidden"}, {UpstreamID: "deleted", Model: "gpt-deleted"}}}
	for _, path := range []string{"state", "cached-routes", "stats"} {
		r := httptest.NewRequest("GET", "/admin/api/"+path, nil)
		r.SetBasicAuth("admin", "correct-horse")
		w := httptest.NewRecorder()
		s.Handle(w, r)
		var data struct {
			Models    []string           `json:"presentation_models"`
			Used      []string           `json:"used_models"`
			Upstreams []*config.Upstream `json:"upstreams"`
			Routes    []CachedRouteEntry `json:"routes"`
		}
		if path == "stats" {
			var stats struct {
				Used []string `json:"used_models"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stats.Used, []string{"gpt-on"}) {
				t.Fatal(stats.Used)
			}
			continue
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(data.Models, []string{"gpt-on"}) {
			t.Fatal(data.Models)
		}
		if path == "state" && (len(data.Upstreams) != 3 || len(data.Upstreams[2].Models) != 1) {
			t.Fatal("operator catalog lost")
		}
		if path == "cached-routes" && len(data.Routes) != 1 {
			t.Fatal("hidden cache leaked")
		}
	}
}
