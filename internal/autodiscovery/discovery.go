// Package autodiscovery reads OmniRoute's declarative catalog, never its executable code.
package autodiscovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const SourceURL = "https://raw.githubusercontent.com/diegosouzapw/OmniRoute/main/open-sse/services/autoCombo/builtinCatalog.ts"
const MaxSourceBytes = 256 << 10

var template = regexp.MustCompile(`(?s)export const AUTO_TEMPLATE_VARIANTS: Record<string, AutoVariant \| undefined> = \{(.*?)\n\};`)
var suffix = regexp.MustCompile(`(?s)export const AUTO_SUFFIX_VARIANTS: string\[\] = \[(.*?)\n\];`)
var entry = regexp.MustCompile(`^"(auto/[a-z][a-z0-9:-]*)": ("(coding|smart|fast|cheap|offline|lkgp|chaos)"|undefined),$`)
var item = regexp.MustCompile(`^"(auto/[a-z][a-z0-9:-]*)",$`)

// Parse accepts only literal entries in the two known exported declarations.
// Any expression or format drift inside them rejects the entire update.
func Parse(data []byte) ([]string, error) {
	if len(data) > MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", MaxSourceBytes)
	}
	seen := map[string]bool{}
	for i, block := range []*regexp.Regexp{template, suffix} {
		matches := block.FindAllSubmatch(data, -1)
		if len(matches) != 1 {
			return nil, fmt.Errorf("unknown OmniRoute catalog format: declaration %d", i)
		}
		count := 0
		for _, line := range strings.Split(string(matches[0][1]), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			re := entry
			if i == 1 {
				re = item
			}
			m := re.FindStringSubmatch(line)
			if m == nil || seen[m[1]] {
				return nil, fmt.Errorf("unknown OmniRoute catalog entry in declaration %d", i)
			}
			seen[m[1]] = true
			count++
		}
		if count == 0 {
			return nil, fmt.Errorf("empty OmniRoute declaration %d", i)
		}
	}
	if !seen["auto/best-coding"] || !seen["auto/best-reasoning"] {
		return nil, fmt.Errorf("OmniRoute catalog missing required anchors")
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Matches permits bare spelling only when that spelling was itself advertised.
func Matches(names []string, advertised string) bool {
	for _, name := range names {
		if advertised == name || advertised == strings.TrimPrefix(name, "auto/") {
			return true
		}
	}
	return false
}

// Cache retains the last successful catalog in memory. Failed attempts are also
// throttled; callers receive the error alongside stale data for observability.
type Cache struct {
	mu      sync.Mutex
	names   []string
	attempt time.Time
	err     error
	client  *http.Client
}

func (c *Cache) Refresh(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.attempt.IsZero() && time.Since(c.attempt) < time.Hour {
		return append([]string(nil), c.names...), c.err
	}
	c.attempt = time.Now()
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SourceURL, nil)
	if err == nil {
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("GitHub catalog HTTP %d", resp.StatusCode)
			} else {
				var data []byte
				data, err = io.ReadAll(io.LimitReader(resp.Body, MaxSourceBytes+1))
				if err == nil {
					var names []string
					names, err = Parse(data)
					if err == nil {
						c.names = names
					}
				}
			}
		}
	}
	c.err = err
	return append([]string(nil), c.names...), err
}
