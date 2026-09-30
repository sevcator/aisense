package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aisense/internal/config"
)

const maxPublicProxySources = 20
const maxPublicProxyCandidates = 500

func (s *Server) fetchPublicProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sources []string `json:"sources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(in.Sources) > maxPublicProxySources {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many public proxy lists"})
		return
	}
	sources := make([]string, 0, len(in.Sources))
	seenSources := map[string]bool{}
	for _, raw := range in.Sources {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed == nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public list URLs must be HTTP(S) URLs without credentials"})
			return
		}
		if !seenSources[parsed.String()] {
			seenSources[parsed.String()] = true
			sources = append(sources, parsed.String())
		}
	}
	if err := s.Cfg.Update(func(c *config.Config) error {
		c.Proxies.PublicSources = sources
		return nil
	}); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	proxies := make([]string, 0)
	seenProxies := map[string]bool{}
	errorsBySource := []string{}
	for index, source := range sources {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, source, nil)
		if err != nil {
			errorsBySource = append(errorsBySource, fmt.Sprintf("List %d: invalid URL", index+1))
			continue
		}
		resp, err := client.Do(request)
		if err != nil {
			errorsBySource = append(errorsBySource, fmt.Sprintf("List %d: fetch failed", index+1))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || readErr != nil || len(body) > 1<<20 {
			errorsBySource = append(errorsBySource, fmt.Sprintf("List %d: HTTP, read, or size failure", index+1))
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			normalized, err := normalizeAdminProxyURL(line)
			if err != nil || seenProxies[normalized] {
				continue
			}
			seenProxies[normalized] = true
			proxies = append(proxies, normalized)
			if len(proxies) >= maxPublicProxyCandidates {
				break
			}
		}
		if len(proxies) >= maxPublicProxyCandidates {
			break
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"proxies": proxies, "sources": sources, "errors": errorsBySource})
}
