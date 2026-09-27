package admin

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

var upstreamImportURLRe = regexp.MustCompile("[a-zA-Z][a-zA-Z0-9+.-]*://[^\\s\"'<>`,;(){}\u201c\u201d]*")
var upstreamImportKeyLabelRe = regexp.MustCompile(`(?i)^(?:(?:api[_ -]?)?key|api[_ -]?token|token|authorization|auth|bearer)(?:\s*[:=]\s*|\s+|$)`)

// upstreamImportURLLabelRe matches a line that starts with an explicit label
// pointing to a base URL / API endpoint, followed by the URL value.
// Handles APIBASE, BASE_URL, ENDPOINT, OPENAI_BASE_URL, OPENAI_API_BASE, API_BASE, etc.
// Captured group 1 is the raw value after the separator.
var upstreamImportURLLabelRe = regexp.MustCompile(`(?i)^(?:api[_ -]?base(?:[_ -]?url)?|base[_ -]?url|endpoint|api[_ -]?url|api[_ -]?host|openai[_ -]?(?:api[_ -]?)?base(?:[_ -]?url)?)(?:\s*[:=]\s*|\s+)(.+)`)

// upstreamImportURLLabelInlineRe finds a URL label that appears mid-line after a key value.
// Used to parse "KEY: x APIBASE: y" single-line format.
var upstreamImportURLLabelInlineRe = regexp.MustCompile(`(?i)\s+(?:api[_ -]?base(?:[_ -]?url)?|base[_ -]?url|endpoint|api[_ -]?url|api[_ -]?host|openai[_ -]?(?:api[_ -]?)?base(?:[_ -]?url)?)(?:\s*[:=]\s*|\s+)`)

// upstreamImportURLLabelSuffixRe matches a trailing URL label (with separator) at the
// end of a string. Used to strip "APIBASE: " from the key part of a mixed-line like
// "KEY: sk-1 APIBASE: https://...".
var upstreamImportURLLabelSuffixRe = regexp.MustCompile(`(?i)\s+(?:api[_ -]?base(?:[_ -]?url)?|base[_ -]?url|endpoint|api[_ -]?url|api[_ -]?host|openai[_ -]?(?:api[_ -]?)?base(?:[_ -]?url)?)(?:\s*[:=]\s*|\s*)$`)

// ImportSkipped never includes pasted values: even a malformed URL may contain secrets.
type ImportSkipped struct {
	URL    string `json:"url"`
	Entry  int    `json:"entry"`
	Reason string `json:"reason"`
}

// ImportResult is the outcome of a mass-import paste.
type ImportResult struct {
	Upstreams []*config.Upstream
	Skipped   []ImportSkipped
	// RawModels[i] holds the raw model route list parsed from a trailing
	// "| a,b,c" segment for Upstreams[i]; nil for formats without one.
	// Internal plumbing for offline import tools, never serialized.
	RawModels [][]string `json:"-"`
}

// ParseImports parses a pasted or file-based multi-entry import text.
func ParseImports(text string) ImportResult { return parseUpstreamImports(text) }

func parseUpstreamImport(text string) (*config.Upstream, error) {
	res := parseUpstreamImports(text)
	if len(res.Upstreams) == 0 {
		return nil, firstSkipError(res)
	}
	return res.Upstreams[0], nil
}

func firstSkipError(res ImportResult) error {
	if len(res.Skipped) > 0 {
		return &importError{res.Skipped[0].Reason}
	}
	return &importError{"could not find upstream URL in pasted text"}
}

type importError struct{ msg string }

func (e *importError) Error() string { return e.msg }

// A URL owns subsequent key lines until the next URL. Blank lines and markdown
// fences do not end that ownership. Unknown text ends it with a sanitized error.
func parseUpstreamImports(text string) ImportResult {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	res := ImportResult{Upstreams: []*config.Upstream{}, Skipped: []ImportSkipped{}}
	skip := func(entry int, reason string) {
		res.Skipped = append(res.Skipped, ImportSkipped{Entry: entry, Reason: reason})
	}
	add := func(rawURL string, keys, models []string, entry int) {
		up, err := newImportedUpstream(rawURL, "")
		if err != nil {
			skip(entry, err.Error())
			return
		}
		up.APIKeys = config.NormalizeAPIKeys(keys)
		if len(up.APIKeys) > 0 {
			up.AuthMode = "swap"
		}
		applyImportModels(up, models)
		res.Upstreams = append(res.Upstreams, up)
		res.RawModels = append(res.RawModels, append([]string(nil), models...))
	}
	lines := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "\ufeff"))
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			lines = append(lines, "")
			continue
		}
		for _, bullet := range []string{"- ", "* ", "+ "} {
			line = strings.TrimPrefix(line, bullet)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	text = strings.TrimSpace(strings.Join(lines, "\n"))
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") && !strings.HasPrefix(text, "[http") {
		var entries []json.RawMessage
		if strings.HasPrefix(text, "{") {
			entries = []json.RawMessage{json.RawMessage(text)}
		} else if json.Unmarshal([]byte(text), &entries) != nil {
			skip(1, "invalid JSON import")
			return res
		}
		for i, entry := range entries {
			var obj map[string]json.RawMessage
			if json.Unmarshal(entry, &obj) != nil || obj == nil {
				skip(i+1, "expected a JSON upstream object")
				continue
			}
			rawURL, keys, invalid := "", []string{}, false
			for _, name := range []string{"base_url", "url", "endpoint"} {
				if value, ok := obj[name]; ok {
					var s string
					if json.Unmarshal(value, &s) != nil || s == "" || rawURL != "" && rawURL != s {
						invalid = true
					}
					rawURL = s
				}
			}
			for _, name := range []string{"api_key", "api_keys", "token"} {
				if value, ok := obj[name]; ok {
					if string(value) == "null" {
						invalid = true
						continue
					}
					var values []string
					if name == "api_keys" {
						invalid = invalid || json.Unmarshal(value, &values) != nil
					} else {
						var s string
						invalid = invalid || json.Unmarshal(value, &s) != nil
						values = []string{s}
					}
					for _, key := range values {
						key = strings.TrimSpace(key)
						if key != "" && !validImportKey(key) {
							invalid = true
						}
						keys = append(keys, key)
					}
				}
			}
			if invalid {
				skip(i+1, "invalid or conflicting JSON fields")
				continue
			}
			add(rawURL, keys, nil, i+1)
		}
		return res
	}
	var rawURL string
	var keys, prefix, models []string
	entry, bad := 0, false
	flush := func() {
		if rawURL != "" && !bad {
			add(rawURL, keys, models, entry)
		}
		rawURL, keys, models, bad = "", nil, nil, false
	}
	for lineIndex, line := range lines {
		if line == "" {
			continue
		}
		matches := upstreamImportURLRe.FindAllStringIndex(line, -1)
		if len(matches) == 0 {
			// Check for an explicit URL label (APIBASE:, BASE_URL:, ENDPOINT:, etc.)
			// with an optional missing scheme (e.g. "APIBASE: api.example.com/v1").
			if sub := upstreamImportURLLabelRe.FindStringSubmatch(line); sub != nil {
				val := strings.TrimSpace(sub[1])
				if labelURL := resolveImportURL(val); labelURL != "" {
					flush()
					rawURL = labelURL
					entry = lineIndex + 1
					keys, prefix = prefix, nil
					continue
				}
			}
			// Check for a URL label mid-line after a key value: "KEY: x APIBASE: y".
			if inlineLoc := upstreamImportURLLabelInlineRe.FindStringIndex(line); inlineLoc != nil {
				urlPart := strings.TrimSpace(line[inlineLoc[1]:])
				if labelURL := resolveImportURL(urlPart); labelURL != "" {
					flush()
					rawURL = labelURL
					entry = lineIndex + 1
					keys, prefix = prefix, nil
					if keyPart := strings.TrimSpace(line[:inlineLoc[0]]); keyPart != "" {
						if k, explicit, kOK := parseImportKey(keyPart); explicit && kOK && k != "" {
							keys = append(keys, k)
						}
					}
					continue
				}
			}
			key, explicit, ok := parseImportKey(line)
			if rawURL != "" {
				if !ok && !bad {
					skip(entry, "unrecognized or empty credential")
					bad = true
				}
				if ok {
					keys = append(keys, key)
				}
			} else if explicit && ok {
				prefix = append(prefix, key)
			} else {
				skip(lineIndex+1, "expected an HTTP(S) URL or labeled credential")
				prefix = nil
			}
			continue
		}
		for i, match := range matches {
			// For the first URL on this line, check whether there is a key value
			// before it (e.g. "KEY: sk-1 APIBASE: https://..."). Strip any trailing
			// URL-label separator and try to parse the remainder as a key.
			if i == 0 && match[0] > 0 {
				before := upstreamImportURLLabelSuffixRe.ReplaceAllString(line[:match[0]], "")
				before = strings.TrimSpace(before)
				// Only add when the key part carries an explicit label (e.g. "KEY:").
				// An unlabeled value (e.g. "base_url:") must not be injected as a key.
				if before != "" {
					if k, explicit, kOK := parseImportKey(before); explicit && kOK && k != "" {
						prefix = append(prefix, k)
					}
				}
			}
			flush()
			rawURL = strings.TrimRight(line[match[0]:match[1]], ".")
			// A closing markdown bracket is not part of the URL; an IPv6 bracket is.
			if strings.Count(rawURL, "]") > strings.Count(rawURL, "[") {
				rawURL = strings.TrimSuffix(rawURL, "]")
			}
			entry = lineIndex + 1
			keys, prefix = prefix, nil
			end := len(line)
			if i+1 < len(matches) {
				end = matches[i+1][0]
			}
			rest := line[match[1]:end]
			// A trailing "| a,b,c" segment carries the raw model route list.
			var entryModels []string
			if bar := strings.Index(rest, "|"); bar >= 0 {
				entryModels = splitImportModels(rest[bar+1:])
				rest = rest[:bar]
			}
			models = entryModels
			tail := strings.Trim(rest, " \t,;`\"'<>()[]{}\u201c\u201d")
			if i+1 < len(matches) {
				for _, label := range []string{"@url", "base_url:", "url:", "URL:", "endpoint:"} {
					tail = strings.TrimSpace(strings.TrimSuffix(tail, label))
				}
			}
			switch {
			case isNoAuthPlaceholder(rest):
				// Explicit no-auth marker: the entry keeps AuthMode none.
			case tail != "":
				key, _, ok := parseImportKey(tail)
				if !ok {
					skip(entry, "unrecognized or empty credential")
					bad = true
				} else {
					keys = append(keys, key)
				}
			}
		}
	}
	flush()
	if len(prefix) > 0 {
		skip(len(lines), "credential has no upstream URL")
	}
	return res
}

func newImportedUpstream(rawURL, apiKey string) (*config.Upstream, error) {
	baseURL := strings.TrimRight(rawURL, "/")
	u, err := url.Parse(baseURL)
	if err != nil || config.ValidateBaseURL(baseURL) != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.ForceQuery {
		return nil, &importError{"invalid upstream URL"}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, &importError{"invalid upstream URL"}
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, &importError{"invalid upstream URL"}
	}
	keys, mode := []string{}, "none"
	if apiKey != "" {
		keys, mode = []string{apiKey}, "swap"
	}
	idBase := sanitizeImportID(u.Host)
	return &config.Upstream{
		ID:       "import-" + idBase,
		Name:     u.Host,
		Type:     "auto",
		BaseURL:  baseURL,
		APIKeys:  keys,
		Models:   []string{"*"},
		Priority: 10,
		Enabled:  true,
		AuthMode: mode,
	}, nil
}

// Automatic imports reuse an existing row's hint for deduplication without
// changing identities of already persisted, possibly dual-stack rows.
func inheritImportHint(up *config.Upstream, existing []*config.Upstream) {
	if up.Type != "auto" && up.Type != "" {
		return
	}
	for _, candidate := range existing {
		if candidate == nil {
			continue
		}
		copy := *up
		copy.Type = candidate.Type
		if identity := config.ExactEndpointIdentity(&copy); identity != "" && identity == config.ExactEndpointIdentity(candidate) {
			up.Type = candidate.Type
			return
		}
	}
}

// isNoAuthPlaceholder recognizes the literal "<no-auth-needed>" credential
// marker exported by some key lists (case-insensitive, optional wrapping).
func isNoAuthPlaceholder(text string) bool {
	return strings.EqualFold(strings.Trim(strings.TrimSpace(text), "`\"',;"), "<no-auth-needed>")
}

// splitImportModels splits a trailing pipe segment into raw model routes.
// Only whitespace and wrapping quotes are trimmed so provider prefixes and
// bracket suffixes survive verbatim.
func splitImportModels(segment string) []string {
	if bar := strings.Index(segment, "|"); bar >= 0 {
		segment = segment[:bar]
	}
	var out []string
	for _, part := range strings.Split(segment, ",") {
		if part = strings.Trim(strings.TrimSpace(part), "`\"'"); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// applyImportModels canonicalizes a raw model route list exactly like
// discovery (modelalias.Build over raw routes with no existing aliases).
// Empty or all-meta lists keep the upstream's default wildcard catalog.
func applyImportModels(up *config.Upstream, rawModels []string) {
	if len(rawModels) == 0 {
		return
	}
	catalog := modelalias.Build(rawModels, nil)
	if len(catalog.Models) == 0 {
		return
	}
	up.Models = catalog.Models
	up.ModelAliases = catalog.Aliases
}

// resolveImportURL returns a fully-qualified https:// URL for val.
// If val already contains "://", it is returned verbatim (after trimming).
// Otherwise https:// is prepended and the result is validated; an invalid
// or empty value returns "".
func resolveImportURL(val string) string {
	val = strings.TrimRight(strings.TrimSpace(val), ".")
	if val == "" {
		return ""
	}
	if strings.Contains(val, "://") {
		// Already has a scheme; strip any trailing markdown punctuation.
		if strings.Count(val, "]") > strings.Count(val, "[") {
			val = strings.TrimSuffix(val, "]")
		}
		if config.ValidateBaseURL(val) == nil {
			return val
		}
		return ""
	}
	// Bare host/path (e.g. "api.example.com/v1" or "192.168.1.1:8080/v1").
	candidate := "https://" + val
	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" || config.ValidateBaseURL(candidate) != nil {
		return ""
	}
	return candidate
}

func parseImportKey(text string) (key string, explicit, ok bool) {
	trimmed := strings.TrimSpace(text)
	if isNoAuthPlaceholder(trimmed) {
		return "", true, true
	}
	if loc := upstreamImportKeyLabelRe.FindStringIndex(trimmed); loc != nil && isNoAuthPlaceholder(strings.TrimSpace(trimmed[loc[1]:])) {
		return "", true, true
	}
	text = strings.Trim(strings.TrimSpace(text), "`\"'<>()[]{}")
	if loc := upstreamImportKeyLabelRe.FindStringIndex(text); loc != nil {
		explicit = true
		text = strings.TrimSpace(text[loc[1]:])
	}
	if explicit && strings.HasPrefix(strings.ToLower(text), "bearer ") {
		text = strings.TrimSpace(text[len("bearer "):])
	}
	text = strings.Trim(text, "`\"'<>()[]{}")
	// Retain loose labeled exports, but never scan prose for a likely credential.
	if !explicit {
		if _, after, found := strings.Cut(text, ": "); found {
			text = strings.TrimSpace(after)
		}
	}
	if !validImportKey(text) {
		return "", explicit, false
	}
	if !explicit && !strings.ContainsAny(text, "0123456789._-/+=") {
		return "", false, false
	}
	return text, explicit, true
}

func validImportKey(key string) bool {
	if key == "" || strings.Contains(key, "://") {
		return false
	}
	for _, r := range key {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-:/+=", r) {
			return false
		}
	}
	return true
}

func sanitizeImportID(host string) string {
	if host == "" {
		return "upstream"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "upstream"
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}

func uniqueUpstreamID(base string, existing []*config.Upstream) string {
	used := map[string]bool{}
	for _, up := range existing {
		if up != nil {
			used[up.ID] = true
		}
	}
	return uniqueIDAmong(base, used)
}

func uniqueIDAmong(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		id := base + "-" + itoaImport(i)
		if !used[id] {
			return id
		}
	}
}

// MergeImportUpstreams collapses parsed import entries into one upstream per
// normalized base URL: every unique key in first-seen order plus the union of
// raw model route lists, canonicalized exactly like discovery. URLs already
// present in existing (compared with config.NormalizeBaseURL) are skipped;
// returned upstreams keep stable "import-<host>-<port>" IDs that never
// collide with existing IDs, use Type openai, Priority 10, enabled, and
// AuthMode swap when keys exist, none otherwise.
func MergeImportUpstreams(res ImportResult, existing []*config.Upstream) []*config.Upstream {
	type bucket struct {
		up      *config.Upstream
		raws    []string
		rawSeen map[string]bool
	}
	seenURL := map[string]bool{}
	usedIDs := map[string]bool{}
	for _, up := range existing {
		if up == nil {
			continue
		}
		if up.BaseURL != "" {
			seenURL[strings.ToLower(config.NormalizeBaseURL(up.BaseURL))] = true
		}
		usedIDs[up.ID] = true
	}
	var order []*bucket
	byURL := map[string]*bucket{}
	for i, up := range res.Upstreams {
		if up == nil || up.BaseURL == "" {
			continue
		}
		norm := strings.ToLower(config.NormalizeBaseURL(up.BaseURL))
		if norm == "" {
			continue
		}
		if b := byURL[norm]; b != nil {
			b.up.APIKeys = config.NormalizeAPIKeys(append(b.up.APIKeys, up.APIKeys...))
			for _, raw := range res.RawModels[i] {
				if !b.rawSeen[raw] {
					b.rawSeen[raw] = true
					b.raws = append(b.raws, raw)
				}
			}
			continue
		}
		if seenURL[norm] {
			continue
		}
		b := &bucket{
			up: &config.Upstream{
				ID:       up.ID,
				Name:     up.Name,
				Type:     "openai",
				BaseURL:  up.BaseURL,
				APIKeys:  append([]string(nil), up.APIKeys...),
				Models:   []string{"*"},
				Priority: 10,
				Enabled:  true,
				AuthMode: "none",
			},
			rawSeen: map[string]bool{},
		}
		if b.up.Name == "" {
			b.up.Name = b.up.ID
		}
		for _, raw := range res.RawModels[i] {
			if !b.rawSeen[raw] {
				b.rawSeen[raw] = true
				b.raws = append(b.raws, raw)
			}
		}
		byURL[norm] = b
		order = append(order, b)
		seenURL[norm] = true
	}
	out := make([]*config.Upstream, 0, len(order))
	for _, b := range order {
		if len(b.up.APIKeys) > 0 {
			b.up.AuthMode = "swap"
		}
		applyImportModels(b.up, b.raws)
		b.up.ID = uniqueIDAmong(b.up.ID, usedIDs)
		usedIDs[b.up.ID] = true
		out = append(out, b.up)
	}
	return out
}

func itoaImport(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
