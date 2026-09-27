package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

type Task struct {
	ID       string   `json:"id"`
	Upstream string   `json:"upstream"`
	Model    string   `json:"model"`
	Routes   []string `json:"advertised_routes"`
	Skip     string   `json:"skip,omitempty"`
	up       *config.Upstream
}
type Attempt struct {
	Route    string `json:"route"`
	Endpoint string `json:"endpoint"`
	Status   int    `json:"http_status,omitempty"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
	MS       int64  `json:"duration_ms"`
	KeySlot  int    `json:"key_slot,omitempty"`
	Cap      int    `json:"output_cap,omitempty"`
}
type Result struct {
	Task
	At       time.Time `json:"at"`
	Outcome  string    `json:"outcome"`
	Reason   string    `json:"reason"`
	Attempts []Attempt `json:"attempts,omitempty"`
}
type Summary struct {
	State      string                    `json:"state"`
	PID        int                       `json:"pid"`
	Started    time.Time                 `json:"started"`
	Updated    time.Time                 `json:"updated"`
	Duration   float64                   `json:"duration_seconds"`
	Total      int                       `json:"total_pairs"`
	Completed  int                       `json:"completed_pairs"`
	Attempted  int                       `json:"attempted_pairs"`
	Requests   int                       `json:"http_requests"`
	Succeeded  int                       `json:"succeeded"`
	Failed     int                       `json:"failed"`
	Timedout   int                       `json:"timedout"`
	Skipped    int                       `json:"skipped"`
	Reasons    map[string]int            `json:"reasons"`
	ByUpstream map[string]map[string]int `json:"by_upstream"`
}

func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
func safe(s string) string {
	if strings.ContainsAny(s, "\r\n\t?@\\") || strings.Contains(s, "://") || len(s) > 180 {
		h := sha256.Sum256([]byte(s))
		return "redacted-" + hex.EncodeToString(h[:8])
	}
	return s
}
func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal("report encoding failed")
	}
	if os.WriteFile(path+".tmp", append(b, '\n'), 0600) != nil {
		fatal("report write failed")
	}
	if os.Rename(path+".tmp", path) != nil {
		fatal("report replace failed")
	}
}
func contains(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func probeType(model string) (string, string) {
	s := strings.ToLower(model)
	if contains(s, "rerank", "ranker") {
		return "", "unsupported_probe_type_rerank_provider_format"
	}
	if contains(s, "embed", "bge-", "e5-", "gte-", "minilm", "jina-clip", "nvclip", "colbert") {
		return "embeddings", ""
	}
	if contains(s, "image", "dall-e", "dalle", "flux", "stable-diffusion", "sdxl", "midjourney", "imagen", "ideogram", "recraft", "seedream", "hunyuan-video", "kolors", "wan2", "wan-", "sora", "veo", "video", "kling", "luma", "runway", "pika", "cogvideo", "audio", "tts", "whisper", "speech", "transcrib", "music", "suno", "realtime", "moderation", "seedance", "nano-banana", "lyria", "kokoro", "orpheus", "chatterbox", "chirp-", "aura-", "eleven-", "fastpitch", "tacotron", "parakeet", "-asr", "scribe-", "dreamshaper", "lucid-origin", "phoenix-1.0", "imagine-", "resnet", "distilbert", "smart-turn", "-live-", "-ocr", "paddleocr", "nemotron-parse", "nemoretriever-parse", "-reward") || s == "nova-3" {
		return "", "unsupported_probe_type_nonchat"
	}
	return "chat/completions", ""
}

func plan(c *config.Config) []Task {
	// Keep configured canonical keys, including variants hidden by the UI. Do not
	// call Candidates/Build: those helpers can synthesize unadvertised spellings.
	all := map[string]bool{}
	for _, u := range c.Upstreams {
		if u == nil || !u.Enabled {
			continue
		}
		for _, m := range u.Models {
			all[m] = true
		}
		for m := range u.ModelAliases {
			all[m] = true
		}
	}
	models := []string{}
	for m := range all {
		models = append(models, m)
	}
	sort.Strings(models)
	var out []Task
	for _, u := range c.Upstreams {
		if u == nil || !u.Enabled {
			continue
		}
		local := map[string]bool{}
		for _, m := range u.Models {
			local[m] = true
		}
		for m := range u.ModelAliases {
			local[m] = true
		}
		for _, m := range models {
			t := Task{Upstream: safe(u.ID), Model: safe(m), up: u}
			h := sha256.Sum256([]byte(u.ID + "\x00" + m))
			t.ID = hex.EncodeToString(h[:16])
			if !local[m] {
				t.Skip = "not_advertised_on_upstream"
			} else if m == "*" {
				t.Skip = "wildcard_unenumerable"
			} else if modelalias.IsMetaName(m) {
				t.Skip = "removed_meta"
			} else {
				routes := u.ModelAliases[m]
				if len(routes) == 0 {
					routes = []string{m}
				}
				seen := map[string]bool{}
				for _, r := range routes {
					if r != "" && !seen[r] {
						seen[r] = true
						t.Routes = append(t.Routes, r)
					}
				}
				if len(t.Routes) == 0 {
					t.Skip = "no_advertised_route"
				}
			}
			out = append(out, t)
		}
	}
	return out
}

func clientFor(c *config.Config, u *config.Upstream) (*http.Client, string) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: c.ModelDiscovery.IgnoreCertErrors}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	// Deliberately never use ProxyFromEnvironment (may contain credentials).
	if u.UseProxy && c.Proxies.Enabled {
		for _, p := range c.Proxies.List {
			if p != nil && p.Working && !p.Excluded {
				v, e := url.Parse(p.URL)
				if e == nil && v.Host != "" && contains(v.Scheme, "http", "socks5") {
					tr.Proxy = http.ProxyURL(v)
					break
				}
			}
		}
		if tr.Proxy == nil {
			return nil, "configured_proxy_unavailable"
		}
	}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, ""
}

func classify(status int, b []byte, endpoint string) (string, string) {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Type    string          `json:"type"`
		Success *bool           `json:"success"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage   `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Stop string `json:"stop_reason"`
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	valid := json.Unmarshal(b, &v) == nil
	logical := valid && ((len(v.Error) > 0 && string(v.Error) != "null" && string(v.Error) != "false") || v.Type == "error" || (v.Success != nil && !*v.Success))
	if status < 200 || status >= 300 || logical {
		s := strings.ToLower(string(b))
		switch {
		case status == 401 || contains(s, "invalid_api_key", "invalid api key", "incorrect api key", "invalid token", "unauthorized", "authentication_error"):
			return "failed", "key_auth"
		case status == 402 || contains(s, "insufficient_quota", "insufficient balance", "credit balance", "quota exceeded"):
			return "failed", "quota_or_billing"
		case status == 429:
			return "failed", "rate_limited"
		case contains(s, "model_not_found", "unknown model", "model not found", "model does not exist", "unsupported model", "no available channel", "model_not_supported"):
			return "failed", "unsupported_or_unavailable_model"
		case contains(s, "provider") && contains(s, "model") && contains(s, "unable to determine"):
			return "failed", "provider_resolution_failed"
		case modelalias.RetryableError(http.StatusOK, s):
			// Use explicit model/provider markers, not the generic HTTP 400
			// heuristic: invalid request parameters do not mean model failure.
			return "failed", "unsupported_or_unavailable_model"
		case contains(s, "max_tokens", "max_completion_tokens", "token limit", "output token"):
			return "failed", "probe_token_parameter_rejected"
		case status == 403:
			return "failed", "access_denied"
		case status >= 500:
			return "failed", "upstream_server_error"
		case status >= 300 && status < 400:
			return "failed", "redirect_not_followed"
		case logical:
			return "failed", "logical_error_envelope"
		default:
			return "failed", "http_request_rejected"
		}
	}
	if !valid {
		if json.Valid(b) {
			return "failed", "unexpected_json_shape"
		}
		for _, line := range bytes.Split(b, []byte{'\n'}) {
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
				return "failed", "unexpected_sse"
			}
		}
		return "failed", "non_json_response"
	}
	if endpoint == "embeddings" {
		if len(v.Data) > 0 && len(v.Data[0].Embedding) > 0 {
			return "succeeded", "embedding_generated"
		}
		return "failed", "invalid_embedding_response"
	}
	for _, x := range v.Choices {
		var text string
		_ = json.Unmarshal(x.Message.Content, &text)
		if strings.TrimSpace(text) != "" || len(x.Message.ToolCalls) > 0 {
			return "succeeded", "generated"
		}
		var parts []struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(x.Message.Content, &parts)
		for _, p := range parts {
			if strings.TrimSpace(p.Text) != "" {
				return "succeeded", "generated"
			}
		}
		if x.Finish == "length" {
			return "succeeded", "accepted_token_cap_empty"
		}
	}
	for _, x := range v.Content {
		if x.Type == "text" && strings.TrimSpace(x.Text) != "" {
			return "succeeded", "generated"
		}
	}
	if v.Stop == "max_tokens" {
		return "succeeded", "accepted_token_cap_empty"
	}
	if len(v.Choices) > 0 || v.Type == "message" {
		return "failed", "accepted_but_empty_response"
	}
	return "failed", "unrecognized_success_envelope"
}

func probe(client *http.Client, u *config.Upstream, route, key string, slot int) Attempt {
	a := Attempt{Route: safe(route), KeySlot: slot}
	endpoint, skip := probeType(route)
	a.Endpoint = endpoint
	if skip != "" {
		a.Outcome = "skipped"
		a.Reason = skip
		return a
	}
	if u.Type != "openai" && u.Type != "anthropic" {
		a.Outcome = "skipped"
		a.Reason = "unsupported_upstream_protocol"
		return a
	}
	payload := map[string]any{"model": route}
	if endpoint == "embeddings" {
		if u.Type != "openai" {
			a.Outcome = "skipped"
			a.Reason = "unsupported_anthropic_embeddings"
			return a
		}
		payload["input"] = "OK"
	} else {
		a.Cap = 64
		payload["messages"] = []map[string]string{{"role": "user", "content": "Reply OK."}}
		payload["stream"] = false
		base := strings.ToLower(modelalias.BaseName(route))
		if u.Type == "anthropic" {
			endpoint = "messages"
			payload["max_tokens"] = 64
		} else if strings.HasPrefix(base, "gpt-5") || strings.HasPrefix(base, "gpt5") || strings.HasPrefix(base, "gpt-6") || strings.HasPrefix(base, "o1") || strings.HasPrefix(base, "o3") || strings.HasPrefix(base, "o4") {
			payload["max_completion_tokens"] = 64
		} else {
			payload["max_tokens"] = 64
		}
	}
	a.Endpoint = endpoint
	base := config.NormalizeBaseURL(u.BaseURL)
	if !strings.HasSuffix(base, "/"+endpoint) {
		base += "/" + endpoint
	}
	v, e := url.Parse(base)
	if e != nil || (v.Scheme != "http" && v.Scheme != "https") || v.Host == "" || v.User != nil || v.RawQuery != "" || v.Fragment != "" {
		a.Outcome = "skipped"
		a.Reason = "unsupported_or_credentialed_base_url"
		return a
	}
	b, _ := json.Marshal(payload)
	req, e := http.NewRequest("POST", base, bytes.NewReader(b))
	if e != nil {
		a.Outcome = "skipped"
		a.Reason = "invalid_request_config"
		return a
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OpenAI/Python 1.99.0")
	if u.Type == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if key != "" {
		if u.Type == "anthropic" {
			req.Header.Set("x-api-key", key)
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	start := time.Now()
	resp, err := client.Do(req)
	a.MS = time.Since(start).Milliseconds()
	if err != nil {
		a.Outcome = "failed"
		a.Reason = "network_or_tls"
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			a.Outcome = "timedout"
			a.Reason = "timeout_20s"
		}
		return a
	}
	defer resp.Body.Close()
	a.Status = resp.StatusCode
	const responseLimit = 2 << 20
	b, err = io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	a.MS = time.Since(start).Milliseconds()
	if err != nil {
		a.Outcome = "failed"
		a.Reason = "response_read_error"
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			a.Outcome = "timedout"
			a.Reason = "timeout_20s"
		}
		return a
	}
	if len(b) > responseLimit {
		a.Outcome, a.Reason = "failed", "response_size_limit_exceeded"
		return a
	}
	a.Outcome, a.Reason = classify(a.Status, b, endpoint)
	return a
}

func main() {
	path := flag.String("config", "config.json", "read-only configuration")
	out := flag.String("out", "", "report directory; same directory resumes")
	inspect := flag.Bool("inspect", false, "sanitized inventory only; no network")
	status := flag.Bool("status", false, "print saved summary; no network or configuration read")
	analyze := flag.String("analyze-log", "", "summarize response categories from a local log; no network or configuration read")
	limit := flag.Duration("budget", 30*time.Minute, "maximum scheduling time per invocation")
	flag.Parse()
	if *analyze != "" {
		if err := analyzeLog(*analyze, os.Stdout); err != nil {
			fatal("log analysis failed")
		}
		return
	}
	if *status {
		b, e := os.ReadFile(filepath.Join(*out, "summary.json"))
		if e != nil {
			fatal("cannot read summary")
		}
		var s Summary
		if json.Unmarshal(b, &s) != nil {
			fatal("invalid summary")
		}
		json.NewEncoder(os.Stdout).Encode(s)
		return
	}
	b, err := config.ReadFile(*path)
	if err != nil {
		fatal("cannot read configuration")
	}
	var c config.Config
	if json.Unmarshal(b, &c) != nil {
		fatal("invalid configuration JSON")
	}
	tasks := plan(&c)
	if *inspect {
		types := map[string]int{}
		auth := map[string]int{}
		models := map[string]bool{}
		kinds := map[string]int{}
		enabled, advertised, aliases, proxied := 0, 0, 0, 0
		for _, u := range c.Upstreams {
			if u != nil && u.Enabled {
				enabled++
				types[safe(u.Type)]++
				auth[safe(u.AuthMode)]++
				if u.UseProxy {
					proxied++
				}
			}
		}
		for _, t := range tasks {
			models[t.Model] = true
			if t.Skip == "" {
				advertised++
				aliases += len(t.Routes)
				for _, r := range t.Routes {
					k, s := probeType(r)
					if s != "" {
						k = s
					}
					kinds[k]++
				}
			}
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"enabled_upstreams": enabled, "total_upstreams": len(c.Upstreams), "models": len(models), "matrix_pairs": len(tasks), "advertised_pairs": advertised, "advertised_raw_routes": aliases, "types": types, "auth_modes": auth, "proxy_requested": proxied, "proxy_enabled": c.Proxies.Enabled, "ignore_cert_errors": c.ModelDiscovery.IgnoreCertErrors, "probe_types": kinds})
		list := []string{}
		for m := range models {
			list = append(list, m)
		}
		sort.Strings(list)
		json.NewEncoder(os.Stdout).Encode(map[string]any{"configured_models": list})
		return
	}
	if *out == "" {
		*out = filepath.Join("model-audit", time.Now().UTC().Format("20060102T150405Z"))
	}
	if os.MkdirAll(*out, 0700) != nil {
		fatal("cannot create report directory")
	}
	lock, e := os.OpenFile(filepath.Join(*out, "audit.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		fatal("audit directory locked; check process before removing stale audit.lock")
	}
	lock.Close()
	defer os.Remove(filepath.Join(*out, "audit.lock"))
	// Refuse to merge a resume with a changed configuration, without persisting it.
	digest := sha256.Sum256(b)
	fingerprint := hex.EncodeToString(digest[:])
	manifest := filepath.Join(*out, "fingerprint.json")
	if old, e := os.ReadFile(manifest); e == nil {
		var s string
		if json.Unmarshal(old, &s) != nil || s != fingerprint {
			fatal("configuration changed; use a new report directory")
		}
	} else {
		writeJSON(manifest, fingerprint)
	}
	// Only sanitized identifiers are serialized; raw route strings stay in memory.
	sanitized := make([]Task, len(tasks))
	for i, t := range tasks {
		sanitized[i] = t
		sanitized[i].Routes = nil
		for _, r := range t.Routes {
			sanitized[i].Routes = append(sanitized[i].Routes, safe(r))
		}
	}
	writeJSON(filepath.Join(*out, "plan.json"), sanitized)
	results := map[string]Result{}
	journal := filepath.Join(*out, "results.jsonl")
	if f, e := os.Open(journal); e == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			var r Result
			if json.Unmarshal(scanner.Bytes(), &r) != nil {
				fatal("incomplete journal line; preserve journal and repair before resume")
			}
			results[r.ID] = r
		}
		if scanner.Err() != nil {
			fatal("journal read failed")
		}
		f.Close()
	}
	f, e := os.OpenFile(journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		fatal("cannot open journal")
	}
	defer f.Close()
	started := time.Now().UTC()
	if old, e := os.ReadFile(filepath.Join(*out, "summary.json")); e == nil {
		var s Summary
		if json.Unmarshal(old, &s) == nil {
			started = s.Started
		}
	}
	var mu sync.Mutex
	lastSummary := time.Time{}
	summarize := func(state string) {
		s := Summary{State: state, PID: os.Getpid(), Started: started, Updated: time.Now().UTC(), Total: len(tasks), Completed: len(results), Reasons: map[string]int{}, ByUpstream: map[string]map[string]int{}}
		s.Duration = s.Updated.Sub(started).Seconds()
		for _, r := range results {
			s.Reasons[r.Reason]++
			if s.ByUpstream[r.Upstream] == nil {
				s.ByUpstream[r.Upstream] = map[string]int{}
			}
			s.ByUpstream[r.Upstream][r.Outcome]++
			s.ByUpstream[r.Upstream]["reason:"+r.Reason]++
			attempted := false
			for _, a := range r.Attempts {
				if a.Outcome != "skipped" {
					s.Requests++
					attempted = true
				}
			}
			if attempted {
				s.Attempted++
			}
			switch r.Outcome {
			case "succeeded":
				s.Succeeded++
			case "failed":
				s.Failed++
			case "timedout":
				s.Timedout++
			case "skipped":
				s.Skipped++
			}
		}
		writeJSON(filepath.Join(*out, "summary.json"), s)
		lastSummary = time.Now()
		md := fmt.Sprintf("# Live Model Audit\n\nState: %s\n\nUpdated: %s; PID: %d; elapsed: %.1f seconds\n\nPairs: %d total, %d completed, %d pending\n\nAttempted: %d; succeeded: %d; failed: %d; timed out: %d; skipped: %d; HTTP requests: %d\n\nSuccess means generated output or explicitly accepted with a token-cap finish; see reason. A 64-token cap can exhaust reasoning before visible output. Timeouts are inconclusive, not proof a model is unavailable. Unadvertised cross-pairs are not probed with fabricated routes. All configured raw aliases are retained and tested, including hidden variants. No circuit breaker: every feasible advertised pair is attempted. Proxy environment is ignored; configured proxy and TLS settings are honored. Response bodies, keys and URLs are never logged.\n", s.State, s.Updated.Format(time.RFC3339), s.PID, s.Duration, s.Total, s.Completed, s.Total-s.Completed, s.Attempted, s.Succeeded, s.Failed, s.Timedout, s.Skipped, s.Requests)
		if os.WriteFile(filepath.Join(*out, "summary.md"), []byte(md), 0600) != nil {
			fatal("markdown write failed")
		}
	}
	record := func(r Result) {
		mu.Lock()
		defer mu.Unlock()
		r.Routes = nil
		r.Task.up = nil // routes are already present in plan.json and attempts
		if json.NewEncoder(f).Encode(r) != nil || f.Sync() != nil {
			fatal("journal persistence failed")
		}
		results[r.ID] = r
		if time.Since(lastSummary) >= 2*time.Second {
			summarize("running")
		}
	}
	groups := map[*config.Upstream][]Task{}
	for _, t := range tasks {
		if _, ok := results[t.ID]; !ok {
			groups[t.up] = append(groups[t.up], t)
		}
	}
	summarize("running")
	deadline := time.Now().Add(*limit)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for u, group := range groups {
		wg.Add(1)
		go func(u *config.Upstream, group []Task) {
			defer wg.Done()
			client, setup := clientFor(&c, u)
			if client != nil {
				defer client.CloseIdleConnections()
			}
			mode := u.AuthMode
			if mode == "" {
				mode = "swap"
			}
			if mode == "passthrough" {
				setup = "passthrough_client_credentials_not_supplied"
			} else if mode == "oauth" {
				setup = "unsupported_oauth_probe"
			} else if mode != "none" && mode != "swap" {
				setup = "unsupported_auth_mode"
			} else if mode == "swap" && len(u.APIKeys) == 0 {
				setup = "swap_requires_client_auth_or_key"
			}
			keyIndex := 0
			for _, t := range group {
				if time.Now().After(deadline) {
					return
				}
				r := Result{Task: t, At: time.Now().UTC()}
				if t.Skip != "" {
					r.Outcome = "skipped"
					r.Reason = t.Skip
					record(r)
					continue
				}
				if setup != "" {
					r.Outcome = "skipped"
					r.Reason = setup
					record(r)
					continue
				}
				for _, route := range t.Routes {
					key := ""
					slot := 0
					if mode == "swap" {
						key = u.APIKeys[keyIndex]
						slot = keyIndex + 1
					}
					sem <- struct{}{}
					a := probe(client, u, route, key, slot)
					<-sem
					r.Attempts = append(r.Attempts, a)
					if a.Reason == "key_auth" && mode == "swap" && keyIndex == 0 && len(u.APIKeys) > 1 {
						keyIndex = 1
						sem <- struct{}{}
						a = probe(client, u, route, u.APIKeys[1], 2)
						<-sem
						r.Attempts = append(r.Attempts, a)
					}
					if r.Outcome != "succeeded" || a.Reason == "generated" || a.Reason == "embedding_generated" {
						r.Outcome = a.Outcome
						r.Reason = a.Reason
					}
				}
				record(r)
			}
		}(u, group)
	}
	wg.Wait()
	state := "paused_budget"
	if len(results) == len(tasks) {
		state = "complete"
	}
	summarize(state)
	fmt.Printf("Audit %s; completed %d/%d pairs; reports: %s\n", state, len(results), len(tasks), *out)
}
