// discoverydiag never loads the config manager or writes application state.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aisense/internal/config"
	"aisense/internal/protocol"
)

func category(s string) string {
	s = strings.ToLower(s)
	for _, p := range []string{"timeout", "deadline", "certificate", "connection refused", "no such host", "eof", "connection reset", "http 401", "http 403", "http 404", "http 429", "invalid models", "no key returned"} {
		if strings.Contains(s, p) {
			return p
		}
	}
	if s != "" {
		return "other"
	}
	return "none"
}

func main() {
	path := flag.String("config", "config.json", "read-only config path")
	logPath := flag.String("log", "", "summarize at most the last 32 MiB of a JSON-lines log")
	probes := flag.Int("probes", 0, "GET catalogs for up to 6 representative enabled hosts")
	flag.Parse()
	raw, err := config.ReadFile(*path)
	if err != nil {
		fmt.Println("config open failed")
		os.Exit(1)
	}
	var c config.Config
	err = json.Unmarshal(raw, &c)
	if err != nil {
		fmt.Println("config decode failed")
		os.Exit(1)
	}
	counts, auth, failures := map[string]int{}, map[string]int{}, map[string]int{}
	for _, u := range c.Upstreams {
		if u == nil {
			continue
		}
		counts["upstreams"]++
		if strings.HasPrefix(u.ID, "import-") {
			counts["imported"]++
		}
		if len(u.APIKeys) == 0 {
			counts["keyless"]++
		}
		if u.Enabled {
			counts["enabled"]++
		}
		switch u.AuthMode {
		case "none", "swap", "oauth", "passthrough", "":
			auth[u.AuthMode]++
		default:
			auth["other"]++
		}
		if u.UseProxy {
			counts["use_proxy"]++
		}
		if u.ModelsRefreshedAt.IsZero() {
			counts["never_refreshed"]++
		}
		if u.ModelsRefreshError != "" {
			failures[category(u.ModelsRefreshError)]++
		}
		n := 0
		for _, m := range u.Models {
			if m == "*" {
				counts["wildcard_rows"]++
			} else {
				n++
			}
		}
		counts["model_entries"] += n
		if n > 0 {
			counts["catalog_rows"]++
		} else {
			counts["no_named_models"]++
		}
	}
	printJSON(map[string]any{"counts": counts, "auth_modes": auth, "errors": failures, "discovery": c.ModelDiscovery, "auto_models_discovery": c.AutoModelsDiscovery, "proxy_enabled": c.Proxies.Enabled, "logging_enabled": c.LoggingEnabled})
	for _, p := range []string{"config.json", "aisense", "aisense.exe", "aisense.next.exe", "main.go", "internal/admin/upstream_models.go", "internal/ui/index.html"} {
		if s, e := os.Stat(p); e == nil {
			printJSON(map[string]any{"file": p, "modified": s.ModTime().UTC(), "bytes": s.Size()})
		}
	}
	if *logPath != "" {
		summarizeLog(*logPath)
	}
	if *probes < 0 || *probes > 6 {
		fmt.Println("probes must be 0..6")
		return
	}
	seen := map[string]bool{}
	for _, u := range c.Upstreams {
		if *probes == 0 {
			break
		}
		if u == nil || !u.Enabled || !strings.HasPrefix(u.ID, "import-") {
			continue
		}
		base, e := url.Parse(u.BaseURL)
		if e != nil || base.User != nil || base.RawQuery != "" || seen[base.Host] {
			continue
		}
		seen[base.Host] = true
		*probes--
		out := map[string]any{"host": base.Host, "auth_mode": u.AuthMode}
		if u.UseProxy || u.AuthMode == "oauth" || u.AuthMode == "passthrough" {
			out["skipped"] = "requires runtime credential/proxy policy"
			printJSON(out)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: c.ModelDiscovery.IgnoreCertErrors}}
		client := &http.Client{Transport: tr, CheckRedirect: protocol.NoRedirect}
		for attempt := 0; attempt < 2; attempt++ {
			req, e := http.NewRequestWithContext(ctx, "GET", protocol.Endpoint(u.BaseURL, "models"), nil)
			if e != nil {
				out["error"] = "invalid URL"
				break
			}
			req.Header.Set("Accept", "application/json")
			if u.AuthMode != "none" && len(u.APIKeys) > 0 {
				anthropic := u.Type == "anthropic"
				if attempt == 1 {
					anthropic = !anthropic
				}
				if anthropic {
					req.Header.Set("X-Api-Key", u.APIKeys[0])
					req.Header.Set("Anthropic-Version", "2023-06-01")
				} else {
					req.Header.Set("Authorization", "Bearer "+u.APIKeys[0])
				}
			}
			resp, e := client.Do(req)
			if e != nil {
				out["error"] = category(e.Error())
				break
			}
			out["status"] = resp.StatusCode
			var payload map[string]json.RawMessage
			e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload)
			resp.Body.Close()
			if e == nil {
				for _, field := range []string{"data", "models"} {
					var items []json.RawMessage
					if json.Unmarshal(payload[field], &items) == nil {
						out[field+"_items"] = len(items)
						if len(items) > 0 {
							var text string
							if json.Unmarshal(items[0], &text) == nil {
								out[field+"_shape"] = "strings"
							} else {
								out[field+"_shape"] = "objects"
							}
						}
					}
				}
			} else {
				out["body_format"] = "not JSON object"
			}
			if attempt == 0 && u.AuthMode != "none" && len(u.APIKeys) > 0 && (resp.StatusCode == 401 || resp.StatusCode == 403) {
				continue
			}
			break
		}
		cancel()
		tr.CloseIdleConnections()
		printJSON(out)
	}
}

func printJSON(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }

func summarizeLog(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("log open failed")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	start := st.Size() - (32 << 20)
	if start < 0 {
		start = 0
	}
	f.Seek(start, io.SeekStart)
	r := bufio.NewReader(io.LimitReader(f, st.Size()-start))
	if start > 0 {
		r.ReadBytes('\n')
	}
	events, statuses, errors := map[string]int{}, map[string]int{}, map[string]int{}
	sessionEvents := map[string]int{}
	lastDiscovery := ""
	var lastStart any
	for {
		line, e := r.ReadBytes('\n')
		if len(line) > 0 {
			var v map[string]any
			if json.Unmarshal(line, &v) == nil {
				event, _ := v["event"].(string)
				switch event {
				case "debug.session.start":
					lastStart = map[string]any{"timestamp": v["timestamp"], "pid": v["pid"]}
					sessionEvents = map[string]int{}
					events[event]++
				case "discovery.request", "discovery.response", "discovery.error", "listener.start", "debug.session.stop":
					events[event]++
					sessionEvents[event]++
					if strings.HasPrefix(event, "discovery.") {
						if text, ok := v["timestamp"].(string); ok {
							if stamp, err := time.Parse(time.RFC3339Nano, text); err == nil {
								lastDiscovery = stamp.UTC().Format(time.RFC3339Nano)
							}
						}
					}
					if n, ok := v["status"].(float64); ok {
						statuses[fmt.Sprintf("%.0f", n)]++
					}
					if s, ok := v["error"].(string); ok {
						errors[category(s)]++
					}
				}
			}
		}
		if e != nil {
			break
		}
	}
	printJSON(map[string]any{"log_bytes": st.Size(), "sample_bytes": st.Size() - start, "events": events, "statuses": statuses, "error_categories": errors, "last_start": lastStart, "events_since_last_start": sessionEvents, "last_discovery_event": lastDiscovery})
}
