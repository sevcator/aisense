package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mathrand "math/rand/v2"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/protocol"
	"aisense/internal/routehealth"
	"aisense/internal/store"
)

type Proxy struct {
	Cfg    *config.Manager
	Store  *store.Store
	Debug  *debuglog.Logger
	Health *routehealth.Manager
	// lastProbe throttles the last-resort attempts on cooling upstreams.
	probeMu   sync.Mutex
	lastProbe map[string]time.Time
	mu        sync.Mutex
	rl        map[string]*window

	clientMu sync.RWMutex
	clients  map[string]*http.Client // pooled per destination (direct / proxy URL)

	upstreamKeyMu sync.Mutex
	upstreamKeys  map[string]*upstreamKeyPoolState
	modelAliasMu  sync.Mutex
	modelAliases  map[string]routeEntry // key: upID|familyKey|thinking

	// cachedUpstreams tracks the valid/working upstream for each model family,
	// keeping it pinned for the configured TTL before re-searching.
	cachedUpstreamMu sync.RWMutex
	cachedUpstreams  map[string]*CachedUpstreamEntry // key: typ + ":" + familyKey

	// usedModels is the set of display-normalised model names that have been
	// served successfully since process start (ephemeral — resets on restart).
	usedModels sync.Map // string → time.Time
}

// routeEntry is a sticky model→route mapping with a creation timestamp so it
// can be expired after StickyUpstreamTTLHours.
type routeEntry struct {
	route    string
	cachedAt time.Time
}

// CachedUpstreamEntry represents a pinned model→upstream cache mapping with TTL.
type CachedUpstreamEntry struct {
	Type       string    `json:"type"`
	Model      string    `json:"model"`
	UpstreamID string    `json:"upstream_id"`
	CachedAt   time.Time `json:"cached_at"`
	Identity   string    `json:"-"`
}

type upstreamKeyPoolState struct {
	identity string // exact endpoint plus authentication context
	cursor   uint64
	cooldown map[string]time.Time
}

type window struct {
	start time.Time
	count int
}

func New(cfg *config.Manager, st *store.Store, debug ...*debuglog.Logger) *Proxy {
	p := &Proxy{
		Cfg:             cfg,
		Store:           st,
		Health:          routehealth.New(),
		rl:              map[string]*window{},
		clients:         map[string]*http.Client{},
		upstreamKeys:    map[string]*upstreamKeyPoolState{},
		modelAliases:    map[string]routeEntry{},
		cachedUpstreams: map[string]*CachedUpstreamEntry{},
		lastProbe:       map[string]time.Time{},
	}
	if len(debug) > 0 {
		p.Debug = debug[0]
	}
	return p
}

type traceContext struct {
	id       string
	started  time.Time
	attempts int
	selected string
}

type traceContextKey struct{}

func newTraceID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func traceFrom(r *http.Request) *traceContext {
	trace, _ := r.Context().Value(traceContextKey{}).(*traceContext)
	return trace
}

func (p *Proxy) trace(r *http.Request, event string, fields map[string]any) {
	if p.Debug == nil {
		return
	}
	traceID := ""
	if trace := traceFrom(r); trace != nil {
		traceID = trace.id
	}
	p.Debug.Event(traceID, event, fields)
}

type traceResponseWriter struct {
	http.ResponseWriter
	proxy  *Proxy
	req    *http.Request
	status int
	bytes  int64
	seq    int
	stream *debuglog.Stream
}

func (w *traceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.proxy.trace(w.req, "client.response", map[string]any{"status": status, "headers": w.proxy.Debug.Headers(w.Header())})
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *traceResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if n > 0 {
		w.seq++
		w.bytes += int64(n)
		if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
			if w.stream == nil {
				w.stream = &debuglog.Stream{Logger: w.proxy.Debug, Emit: func(body any) {
					w.proxy.trace(w.req, "client.response_chunk", map[string]any{"status": w.status, "body": body})
				}}
			}
			w.stream.Write(body[:n])
			return n, err
		}
		w.proxy.trace(w.req, "client.response_chunk", map[string]any{
			"sequence": w.seq, "status": w.status, "body": w.proxy.Debug.Body(body[:n], w.Header().Get("Content-Type")),
		})
	}
	return n, err
}

func (w *traceResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// error helpers

func jsonErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

func anthropicErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func modelUnavailableMsg(model string) string {
	return "the requested model " + model + " is unavailable"
}

// attemptSummary collects why the upstreams could not serve a model. The client
// only hears "unavailable"; the reasons go to the server log and the trace.
type attemptSummary struct {
	upstreams map[string]bool
	reasons   map[string]int
}

func (a *attemptSummary) add(upstreamID, reason string) {
	if a == nil {
		return
	}
	if a.upstreams == nil {
		a.upstreams, a.reasons = map[string]bool{}, map[string]int{}
	}
	a.upstreams[upstreamID] = true
	a.reasons[reason]++
}

// Plain words for the categories the policy produces.
var reasonWords = map[string]string{
	"auth":       "rejected key",
	"limit":      "spent quota",
	"rate_limit": "rate limit",
	"model":      "model not served there",
	"outage":     "API base down",
	"cooling":    "still cooling down",
	"request":    "request refused",
	"transient":  "API base down",
	"quota":      "spent quota",
}

func (a *attemptSummary) String() string {
	if a == nil || len(a.upstreams) == 0 {
		return ""
	}
	words := make([]string, 0, len(a.reasons))
	for reason, count := range a.reasons {
		word := reasonWords[reason]
		if word == "" {
			word = reason
		}
		if count > 1 {
			word += " ×" + strconv.Itoa(count)
		}
		words = append(words, word)
	}
	sort.Strings(words)
	return " (tried " + strconv.Itoa(len(a.upstreams)) + " upstreams: " + strings.Join(words, ", ") + ")"
}

// ---------------------------------------------------------------------------
// rate limiting

func (p *Proxy) rateCheck(key *config.APIKey) (bool, string) {
	if key.RPM <= 0 && key.RPD <= 0 && key.TPD <= 0 {
		return true, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, lim := range []struct {
		n    int
		name string
		span time.Duration
	}{
		{key.RPM, "rpm", time.Minute},
		{key.RPD, "rpd", 24 * time.Hour},
	} {
		if lim.n <= 0 {
			continue
		}
		w := p.rl[key.ID+"|"+lim.name]
		if w == nil || now.Sub(w.start) >= lim.span {
			w = &window{start: now, count: 1}
			p.rl[key.ID+"|"+lim.name] = w
		} else if w.count >= lim.n {
			return false, lim.name
		} else {
			w.count++
		}
	}
	if key.TPD > 0 {
		in, out := p.Store.DayTokens(key.ID)
		if in+out >= int64(key.TPD) {
			return false, "tpd"
		}
	}
	return true, ""
}

func allowedModel(key *config.APIKey, model string) bool {
	if len(key.AllowedModels) == 0 {
		return true
	}
	for _, m := range key.AllowedModels {
		if m == "*" || modelalias.Matches(m, model) {
			return true
		}
	}
	return false
}

func (p *Proxy) findKey(r *http.Request) *config.APIKey {
	cred := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if cred == "" {
		cred = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if cred == "" {
		return nil
	}
	for _, k := range p.Cfg.Get().APIKeys {
		if k.Key == cred {
			return k
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// routing

func (p *Proxy) eligibleCandidates(typ, model string, preferThinking bool, boost []string) []*config.Upstream {
	cfg := p.Cfg.Get()
	var out []*config.Upstream
	for _, up := range cfg.Upstreams {
		if !up.Enabled {
			continue
		}
		if _, ok := resolveUpstreamModelBoost(up, model, preferThinking, boost); ok {
			out = append(out, up)
		}
	}
	return out
}

func (p *Proxy) candidates(typ, model string, preferThinking bool, boost []string) []*config.Upstream {
	cands := p.Health.Order(p.eligibleCandidates(typ, model, preferThinking, boost), model)
	variantRank := func(up *config.Upstream) int {
		for _, route := range modelalias.BoostedVariantCandidates(up.Models, up.ModelAliases, model, preferThinking, boost) {
			if upstreamModelBlocked(up, route) {
				continue
			}
			for i, suffix := range boost {
				if modelalias.Matches(route, model+"-"+suffix) {
					return i
				}
			}
		}
		return len(boost)
	}
	if len(cands) > 1 && len(boost) > 0 {
		sort.SliceStable(cands, func(i, j int) bool { return variantRank(cands[i]) < variantRank(cands[j]) })
	}
	if len(cands) < 2 {
		return p.hitchanceOrder(cands, model, boost)
	}
	// Do not promote the last successful upstream ahead of the health/load
	// ordering. A global sticky route pins every client to one provider, defeats
	// round-robin ties and makes parallel traffic overload that provider even
	// while equivalent upstreams are idle. Health.Order already rotates equal
	// candidates and accounts for in-flight requests; keep its ordering intact.
	return p.hitchanceOrder(cands, model, boost)
}

func userModelName(model string) string {
	return modelalias.BaseName(model)
}

// resolveUpstreamModel maps a requested model onto one of the upstream's
// models. Aliases are matched leniently: provider prefixes, '.' vs '-', and
// the -thinking suffix are ignored for matching. preferThinking ranks
// -thinking variants first; exact (prefix-stripped) string matches win
// within the same class. A "*" wildcard passes the request through.
func resolveUpstreamModel(up *config.Upstream, requested string, preferThinking bool) (string, bool) {
	return resolveUpstreamModelBoost(up, requested, preferThinking, nil)
}

// resolveUpstreamModelBoost additionally prefers specialized variant routes
// (e.g. -low for reasoning_effort=low, -fast in fast mode) when the upstream
// actually serves such a variant family.
func resolveUpstreamModelBoost(up *config.Upstream, requested string, preferThinking bool, boost []string) (string, bool) {
	if upstreamModelBlocked(up, requested) {
		return "", false
	}
	if candidates := modelalias.CandidatesBoost(up.Models, up.ModelAliases, requested, preferThinking, boost); len(candidates) > 0 {
		return candidates[0], true
	}
	return "", false
}

// displayModelName renders a model for user-facing listings: provider prefix
// stripped, -thinking collapsed into the base name, and single-digit
// hyphen-separated version segments turned into dots (claude-opus-4-6 ->
// claude-opus-4.6).
func displayModelName(m string) string {
	return modelalias.DisplayName(m)
}

func upstreamModelBlocked(up *config.Upstream, requested string) bool {
	for _, blocked := range up.BlockedModels {
		if blocked == "*" || modelalias.Matches(blocked, requested) {
			return true
		}
	}
	return false
}

// Resolve across the complete enabled catalog, before health/policy filtering:
// a blocked exact route must not cause substitution onto a different family.
func (p *Proxy) fuzzyRoutingModel(typ, requested string) (string, bool) {
	var names []string
	for _, up := range p.Cfg.Get().Upstreams {
		if !up.Enabled {
			continue
		}
		var explicit []string
		for _, model := range up.Models {
			if strings.TrimSpace(model) != "*" {
				explicit = append(explicit, model)
			}
		}
		if len(explicit) == 0 {
			continue
		}
		if len(modelalias.Candidates(explicit, up.ModelAliases, requested, false)) > 0 {
			return "", false
		}
		for _, model := range explicit {
			if routes := up.ModelAliases[model]; len(routes) > 0 {
				names = append(names, routes...)
			} else {
				names = append(names, model)
			}
		}
	}
	return modelalias.FuzzyMatch(names, requested)
}

// Fuzzy routing only uses advertised raw routes, never synthesized thinking or
// effort variants, and never lets a wildcard evade the selected target policy.
func fuzzyModelRoutes(up *config.Upstream, key *config.APIKey, requested, target string) []string {
	if !allowedModel(key, requested) || !allowedModel(key, target) ||
		upstreamModelBlocked(up, requested) || upstreamModelBlocked(up, target) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, model := range up.Models {
		routes := up.ModelAliases[model]
		if len(routes) == 0 {
			routes = []string{model}
		}
		for _, raw := range routes {
			if !modelalias.Matches(raw, target) || seen[raw] || upstreamModelBlocked(up, model) ||
				upstreamModelBlocked(up, raw) || !allowedModel(key, model) || !allowedModel(key, raw) {
				continue
			}
			if _, ok := modelalias.FuzzyMatch([]string{raw}, requested); ok {
				out = append(out, raw)
				seen[raw] = true
				if len(out) == modelalias.MaxCandidates {
					return out
				}
			}
		}
	}
	return out
}

func (p *Proxy) writeModelsList(w http.ResponseWriter, typ string, key *config.APIKey) {
	type modelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var names []string
	for _, up := range p.Cfg.Get().Upstreams {
		if !up.ModelsVisible() {
			continue
		}
		for _, raw := range up.VisibleModelNames() {
			model := displayModelName(raw)
			if model == "" || model == "*" || modelalias.IsMetaName(model) || !allowedModel(key, model) {
				continue
			}
			names = append(names, raw)
		}
	}
	options := p.Cfg.Get().Models.VariantOptions()
	items := []modelItem{}
	for _, model := range modelalias.PresentationNames(names, options) {
		items = append(items, modelItem{ID: model, Object: "model", Created: 0, OwnedBy: "aisense"})
	}
	for _, combo := range p.comboNamesFor(key) {
		items = append(items, modelItem{ID: combo, Object: "model", Created: 0, OwnedBy: "aisense"})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
}

func isModelsListPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return path == "/models" || path == "/v1/models" || path == "/model" || path == "/v1/model"
}

func modelIDFromRetrievePath(path string) (string, bool) {
	path = strings.TrimSuffix(path, "/")
	for _, prefix := range []string{"/v1/models/", "/models/", "/v1/model/", "/model/"} {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return userModelName(path[len(prefix):]), true
		}
	}
	return "", false
}

func isModelEndpointPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return path == "/model" || path == "/v1/model" || strings.HasPrefix(path, "/model/") || strings.HasPrefix(path, "/v1/model/")
}

// comboNamesFor lists the combos this API key may use: either the key allows
// the combo name itself, or it allows at least one model the combo stands for.
func (p *Proxy) comboNamesFor(key *config.APIKey) []string {
	cfg := p.Cfg.Get()
	out := []string{}
	for _, name := range cfg.ComboNames() {
		if allowedModel(key, name) || anyAllowedModel(key, cfg.ComboModels(name)) {
			out = append(out, name)
		}
	}
	return out
}

func anyAllowedModel(key *config.APIKey, models []string) bool {
	for _, model := range models {
		if allowedModel(key, model) {
			return true
		}
	}
	return false
}

func (p *Proxy) writeModelObject(w http.ResponseWriter, key *config.APIKey, model string) bool {
	if model == "" || modelalias.IsMetaName(model) {
		return false
	}
	for _, combo := range p.comboNamesFor(key) {
		if strings.EqualFold(combo, model) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": combo, "object": "model", "created": 0, "owned_by": "aisense"})
			return true
		}
	}
	if !allowedModel(key, model) {
		return false
	}
	for _, up := range p.Cfg.Get().Upstreams {
		if !up.ModelVisible(model) {
			continue
		}
		for _, raw := range up.Models {
			if modelalias.Matches(raw, model) && !upstreamModelBlocked(up, model) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "aisense"})
				return true
			}
		}
	}
	return false
}

func (p *Proxy) stickyTTL() time.Duration {
	h := p.Cfg.Get().Hitchance.StickyUpstreamTTLHours
	if h <= 0 {
		h = 24
	}
	return time.Duration(h) * time.Hour
}

func (p *Proxy) upstreamCacheTTL() time.Duration {
	h := p.Cfg.Get().Hitchance.UpstreamCacheTTLHours
	if h <= 0 {
		h = 24
	}
	return time.Duration(h) * time.Hour
}

// routeStateKey identifies a sticky model→route mapping. The boost class is
// part of the key so a route remembered for reasoning_effort=low never leaks
// into an effort=high request.
func routeStateKey(upID, requested string, preferThinking bool, boost []string) string {
	return upID + "|" + modelalias.FamilyKey(requested) + "|" + strconv.FormatBool(preferThinking) + "|" + strings.Join(boost, ",")
}

func (p *Proxy) modelRouteCandidates(up *config.Upstream, requested string, preferThinking bool, boost []string) []string {
	candidates := modelalias.CandidatesBoost(up.Models, up.ModelAliases, requested, preferThinking, boost)
	if len(candidates) < 2 {
		return candidates
	}
	stateKey := routeStateKey(up.ID, requested, preferThinking, boost)
	p.modelAliasMu.Lock()
	entry := p.modelAliases[stateKey]
	// Expire sticky entry if TTL has passed.
	if entry.route != "" && time.Since(entry.cachedAt) > p.stickyTTL() {
		delete(p.modelAliases, stateKey)
		entry = routeEntry{}
	}
	p.modelAliasMu.Unlock()
	if entry.route == "" || candidates[0] == entry.route {
		return candidates
	}
	for i, candidate := range candidates[1:] {
		if candidate == entry.route {
			copy(candidates[1:i+2], candidates[0:i+1])
			candidates[0] = entry.route
			break
		}
	}
	return candidates
}

func (p *Proxy) rememberModelRoute(up *config.Upstream, requested string, preferThinking bool, boost []string, raw string) {
	if raw == "" {
		return
	}
	stateKey := routeStateKey(up.ID, requested, preferThinking, boost)
	p.modelAliasMu.Lock()
	if p.modelAliases == nil {
		p.modelAliases = map[string]routeEntry{}
	}
	p.modelAliases[stateKey] = routeEntry{route: raw, cachedAt: time.Now()}
	p.modelAliasMu.Unlock()
}

// forgetModelRoute clears the sticky route remembered by rememberModelRoute for
// every thinking/variant class. Called when all model aliases for an upstream
// are exhausted so the next request does not lead with the broken route.
func (p *Proxy) forgetModelRoute(up *config.Upstream, requested string) {
	prefix := up.ID + "|" + modelalias.FamilyKey(requested) + "|"
	p.modelAliasMu.Lock()
	for key := range p.modelAliases {
		if strings.HasPrefix(key, prefix) {
			delete(p.modelAliases, key)
		}
	}
	p.modelAliasMu.Unlock()
}

func upstreamCacheKey(typ, model string) string {
	return typ + ":" + modelalias.FamilyKey(model)
}

func (p *Proxy) getCachedUpstream(typ, model string) *config.Upstream {
	key := upstreamCacheKey(typ, model)
	ttl := p.upstreamCacheTTL()
	now := time.Now()

	p.cachedUpstreamMu.RLock()
	entry := p.cachedUpstreams[key]
	p.cachedUpstreamMu.RUnlock()

	if entry == nil {
		return nil
	}
	if now.Sub(entry.CachedAt) > ttl {
		p.cachedUpstreamMu.Lock()
		delete(p.cachedUpstreams, key)
		p.cachedUpstreamMu.Unlock()
		return nil
	}

	cfg := p.Cfg.Get()
	for _, up := range cfg.Upstreams {
		if up.ID == entry.UpstreamID && up.Enabled {
			return up
		}
	}
	return nil
}

func (p *Proxy) setCachedUpstream(typ, model string, up *config.Upstream) {
	if up == nil {
		return
	}
	key := upstreamCacheKey(typ, model)
	displayName := displayModelName(model)
	if displayName == "" || displayName == "*" {
		displayName = userModelName(model)
	}
	entry := &CachedUpstreamEntry{
		Type:       typ,
		Model:      displayName,
		UpstreamID: up.ID,
		CachedAt:   time.Now(),
		Identity:   up.HitchanceIdentity(),
	}
	p.cachedUpstreamMu.Lock()
	if p.cachedUpstreams == nil {
		p.cachedUpstreams = map[string]*CachedUpstreamEntry{}
	}
	p.cachedUpstreams[key] = entry
	p.cachedUpstreamMu.Unlock()
	if err := p.Cfg.RecordCachedRoute(up, typ, displayName); err != nil {
		log.Printf("[aisense] could not save cached route: %v", err)
	}
}

// recordUsedModel records a successfully-served model name in the in-process
// used-models set (Feature 1).
func (p *Proxy) recordUsedModel(model string) {
	displayName := displayModelName(model)
	if displayName == "" || displayName == "*" {
		displayName = userModelName(model)
	}
	if displayName == "" || displayName == "*" {
		return
	}
	p.usedModels.LoadOrStore(displayName, time.Now())
}

// UsedModels returns the display-normalised names of models that have been
// served at least once since process start, sorted alphabetically.
func (p *Proxy) UsedModels() []string {
	var out []string
	p.usedModels.Range(func(k, _ any) bool {
		for _, up := range p.Cfg.Get().Upstreams {
			if up.ModelVisible(k.(string)) {
				out = append(out, k.(string))
				break
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// CachedUpstreams returns entries from the upstream cache that are
// still within the TTL. Stale entries are purged during this call.
func (p *Proxy) CachedUpstreams() []CachedUpstreamEntry {
	ttl := p.upstreamCacheTTL()
	cfg := p.Cfg.Get()
	owners := make(map[string]*config.Upstream, len(cfg.Upstreams))
	for _, up := range cfg.Upstreams {
		if up != nil && up.Enabled {
			owners[up.ID] = up
		}
	}
	now := time.Now()
	entries := map[string]CachedUpstreamEntry{}
	add := func(entry CachedUpstreamEntry) {
		up := owners[entry.UpstreamID]
		if up == nil || entry.Identity != "" && entry.Identity != up.HitchanceIdentity() || now.Sub(entry.CachedAt) > ttl || !up.ModelVisible(entry.Model) {
			return
		}
		key := entry.Type + ":" + modelalias.FamilyKey(entry.Model) + ":" + entry.UpstreamID
		if old, ok := entries[key]; !ok || entry.CachedAt.After(old.CachedAt) {
			entries[key] = entry
		}
	}
	for _, saved := range cfg.CachedRoutes {
		add(CachedUpstreamEntry{Type: saved.Type, Model: saved.Model, UpstreamID: saved.UpstreamID, CachedAt: saved.CachedAt, Identity: saved.Identity})
	}
	p.cachedUpstreamMu.Lock()
	defer p.cachedUpstreamMu.Unlock()
	for k, e := range p.cachedUpstreams {
		if now.Sub(e.CachedAt) > ttl {
			delete(p.cachedUpstreams, k)
			continue
		}
		add(*e)
	}
	out := make([]CachedUpstreamEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].UpstreamID < out[j].UpstreamID
	})
	return out
}

func (p *Proxy) combinedModelRoutes(up *config.Upstream, requested string, boost []string) []string {
	out := make([]string, 0, modelalias.MaxCandidates)
	seen := map[string]bool{}
	if modelalias.IsThinking(requested) {
		for _, route := range modelalias.ListingNames(up.Models, up.ModelAliases) {
			if strings.EqualFold(userModelName(route), userModelName(requested)) && !seen[route] {
				seen[route] = true
				out = append(out, route)
			}
		}
	}
	for _, route := range modelalias.BoostedVariantCandidates(up.Models, up.ModelAliases, requested, false, boost) {
		if !upstreamModelBlocked(up, route) && !seen[route] {
			seen[route] = true
			out = append(out, route)
		}
	}
	if route := p.rememberedModelRoute(up, requested, boost); route != "" {
		if !seen[route] {
			seen[route] = true
			out = append(out, route)
		}
	}
	forced := modelalias.ForcedCandidates(requested)
	for _, route := range forced {
		if !seen[route] && len(out) < modelalias.MaxCandidates {
			seen[route] = true
			out = append(out, route)
		}
	}
	passes := []bool{false}
	if len(forced) > 0 || modelalias.SupportsThinking(up.Models, up.ModelAliases, requested) {
		passes = []bool{true, false}
	}
	for _, preferThinking := range passes {
		for _, route := range p.modelRouteCandidates(up, requested, preferThinking, boost) {
			if !seen[route] && len(out) < modelalias.MaxCandidates {
				seen[route] = true
				out = append(out, route)
			}
		}
	}
	return out
}

func (p *Proxy) rememberedModelRoute(up *config.Upstream, requested string, boost []string) string {
	valid := map[string]bool{}
	for _, route := range modelalias.ForcedCandidates(requested) {
		valid[route] = true
	}
	for _, preferThinking := range []bool{true, false} {
		for _, route := range modelalias.CandidatesBoost(up.Models, up.ModelAliases, requested, preferThinking, boost) {
			valid[route] = true
		}
	}
	p.modelAliasMu.Lock()
	defer p.modelAliasMu.Unlock()
	for _, preferThinking := range []bool{true, false} {
		key := routeStateKey(up.ID, requested, preferThinking, boost)
		entry := p.modelAliases[key]
		if entry.route == "" || !valid[entry.route] {
			delete(p.modelAliases, key)
			continue
		}
		if time.Since(entry.cachedAt) > p.stickyTTL() {
			delete(p.modelAliases, key)
			continue
		}
		return entry.route
	}
	return ""
}

func retryAfterCooldown(header http.Header) time.Duration {
	duration := time.Minute
	if retryAfter := strings.TrimSpace(header.Get("Retry-After")); retryAfter != "" {
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
			duration = time.Duration(seconds) * time.Second
		} else if when, err := http.ParseTime(retryAfter); err == nil && when.After(time.Now()) {
			duration = time.Until(when)
		}
	}
	return duration
}

// upstreamURL maps the client path onto the upstream base URL.
// If baseURL already ends with the remainder (OmniRoute-style full endpoint
// URLs like https://api.deepseek.com/responses), use it verbatim.
func upstreamURL(base, prefix, clientPath string) string {
	return protocol.Endpoint(config.NormalizeBaseURL(base), strings.TrimPrefix(strings.TrimPrefix(clientPath, prefix), "/"))
}

// ---------------------------------------------------------------------------
// forwarding

type forwardResult struct {
	// Original provider evidence is distinct from a translated client error.
	// Nil leaves native responses unchanged; local failures never populate it.
	upstreamEvidence *hitchance.Input
	localFailure     bool // validation/conversion output is not upstream evidence
	hitchance        hitchance.Decision
	streamFailed     bool
	status           int
	header           http.Header
	body             []byte
	committed        bool // true when the response was already streamed to the client
}

func hasLogicalErrorEnvelope(body []byte) bool {
	return hitchance.HasErrorEnvelope(body)
}

func logicalErrorStatus(body []byte) int {
	lower := hitchance.ErrorText(body)
	if modelalias.RetryableError(http.StatusBadRequest, lower) {
		return http.StatusNotFound
	}
	for _, marker := range []string{"rate limit", "too many requests"} {
		if strings.Contains(lower, marker) {
			return http.StatusTooManyRequests
		}
	}
	for _, marker := range []string{"unauthorized", "authentication", "invalid api key", "invalid_api_key", "your key is invalid", "need to sign in", "not signed in"} {
		if strings.Contains(lower, marker) {
			return http.StatusUnauthorized
		}
	}
	for _, marker := range []string{"unavailable", "inactive", "overloaded", "overload", "temporarily unavailable"} {
		if strings.Contains(lower, marker) {
			return http.StatusServiceUnavailable
		}
	}
	return http.StatusBadGateway
}

func readFirstSSEFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	var first bytes.Buffer
	for first.Len() < limit {
		line, err := reader.ReadBytes('\n')
		first.Write(line)
		if len(bytes.TrimSpace(line)) == 0 && bytes.Contains(first.Bytes(), []byte("data:")) {
			return first.Bytes(), err
		}
		if err != nil {
			return first.Bytes(), err
		}
	}
	return first.Bytes(), nil
}

type responseStartGuard struct {
	once     sync.Once
	timer    *time.Timer
	cancel   context.CancelFunc
	timedOut atomic.Bool
}

func (g *responseStartGuard) started() {
	g.once.Do(func() { g.timer.Stop() })
}

func (g *responseStartGuard) timeout() {
	g.once.Do(func() {
		g.timedOut.Store(true)
		g.cancel()
	})
}

type guardedResponseBody struct {
	io.ReadCloser
	guard *responseStartGuard
}

func (b *guardedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.guard.started()
	return n, err
}

func (b *guardedResponseBody) Close() error {
	b.guard.started()
	b.guard.cancel()
	return b.ReadCloser.Close()
}

func (p *Proxy) doWithResponseStartTimeout(req *http.Request, useProxy bool) (*http.Response, error) {
	seconds := p.Cfg.Get().Hitchance.ResponseStartTimeoutSeconds
	if seconds < 1 {
		seconds = 20
	}
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	guard := &responseStartGuard{cancel: cancel}
	guard.timer = time.AfterFunc(time.Duration(seconds)*time.Second, guard.timeout)
	resp, err := p.httpClientFor(useProxy).Do(req)
	if err != nil {
		guard.started()
		cancel()
		if guard.timedOut.Load() {
			return nil, fmt.Errorf("response start timeout after %ds", seconds)
		}
		return nil, err
	}
	resp.Body = &guardedResponseBody{ReadCloser: resp.Body, guard: guard}
	return resp, nil
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "TE": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true,
}

// forwardOnce sends one request. Non-stream responses are fully buffered and
// NOT written to the client (caller decides after failover logic). Streams
// commit to w immediately once data starts flowing.
// The request and response pass through byte-for-byte: every header the
// client sent is forwarded (minus hop-by-hop), the body is untouched, and the
// response body+headers go back verbatim (DisableCompression keeps
// Content-Encoding intact). Only the credential header is swapped per
// upstream auth mode.
// buildUpstreamRequest assembles the outgoing request for one attempt. When
// sanitize is non-nil (Antigravity-backed route), client-identifying headers
// are scrubbed and a neutral official User-Agent is set instead; credential
// headers stay under the control of the per-upstream auth mode below.
func (p *Proxy) buildUpstreamRequest(up *config.Upstream, typ string, r *http.Request, body []byte, url, upstreamKey string, sanitize *antigravity) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// full header passthrough (minus hop-by-hop)
	for k, vs := range r.Header {
		if hopHeaders[textproto.CanonicalMIMEHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if sanitize != nil {
		scrubClientHeaders(req.Header)
		req.Header.Set("User-Agent", officialUserAgent)
	}
	if shouldNormalizeUserAgent(req) {
		// Some upstreams fronted by Cloudflare (notably OpenCode Go) reject Go's
		// default transport fingerprint and common script UAs with 1010. Use a
		// normal OpenAI SDK UA only for that upstream/fingerprint class.
		req.Header.Set("User-Agent", "OpenAI/Python 1.99.0")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	// credential handling per auth mode
	switch authMode(up) {
	case "passthrough":
		// keep client headers exactly as received
	case "none":
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	case "oauth":
		tok, err := p.oauthBearer(r.Context(), up)
		if err != nil {
			return nil, fmt.Errorf("oauth: %w", err)
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		if typ == "anthropic" {
			req.Header.Set("x-api-key", tok)
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	default: // "swap"
		if upstreamKey != "" {
			req.Header.Del("Authorization")
			req.Header.Del("x-api-key")
			if typ == "anthropic" {
				req.Header.Set("x-api-key", upstreamKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+upstreamKey)
			}
		}
		// empty key pool + swap -> passthrough (web/cookie providers)
	}
	if typ == "anthropic" && req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	req.Header.Del("Content-Length")
	return req, nil
}

// forwardNative sends one request over the client's native wire format.
// Responses pass through byte-for-byte, except on Antigravity-backed routes
// (san non-nil) where tool names are restored in buffered bodies and SSE
// frames are piped through a line-buffered remapper before reaching the
// client. Non-stream responses are fully buffered and NOT written to the
// client (caller decides after failover logic). Streams commit to w
// immediately once data starts flowing.
func (p *Proxy) forwardNative(up *config.Upstream, typ, prefix string, r *http.Request, body []byte, capture *bytes.Buffer, w http.ResponseWriter, upstreamKey string, san *antigravity) (*forwardResult, error) {
	url := upstreamURL(up.BaseURL, prefix, r.URL.Path)
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	doOnce := func(u string) (*http.Response, error) {
		req, err := p.buildUpstreamRequest(up, typ, r, body, u, upstreamKey, san)
		if err != nil {
			return nil, err
		}
		p.trace(r, "upstream.request", map[string]any{
			"upstream_id": up.ID, "auth_mode": authMode(up), "method": req.Method,
			"url": p.Debug.URL(req.URL.String()), "headers": p.Debug.Headers(req.Header),
			"body": p.Debug.Body(body, req.Header.Get("Content-Type")),
		})
		return p.doWithResponseStartTimeout(req, up.UseProxy)
	}
	resp, err := doOnce(url)
	if err != nil {
		p.trace(r, "upstream.error", map[string]any{
			"upstream_id": up.ID, "method": r.Method, "url": p.Debug.URL(url), "error": err.Error(),
			"input": p.Debug.Body(body, r.Header.Get("Content-Type")),
		})
		return nil, err
	}
	defer resp.Body.Close()
	p.trace(r, "upstream.response", map[string]any{
		"upstream_id": up.ID, "status": resp.StatusCode, "headers": p.Debug.Headers(resp.Header),
	})

	streaming := isStreaming(body)
	fr := &forwardResult{status: resp.StatusCode, header: resp.Header.Clone()}

	if !streaming {
		fr.body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if san != nil {
			fr.body = san.restoreResponse(fr.body)
		}
		if fr.status >= 200 && fr.status < 300 && hasLogicalErrorEnvelope(fr.body) {
			fr.status = logicalErrorStatus(fr.body)
			p.trace(r, "upstream.logical_error", map[string]any{"upstream_id": up.ID, "status": fr.status, "body": p.Debug.Body(fr.body, resp.Header.Get("Content-Type"))})
		} else {
			p.trace(r, "upstream.response_body", map[string]any{"upstream_id": up.ID, "body": p.Debug.Body(fr.body, resp.Header.Get("Content-Type"))})
		}
		fr = p.autofixHTTPS(up, prefix, r, body, fr, doOnce)
		if capture != nil {
			capture.Write(fr.body)
		}
		return fr, err
	}

	// Streaming requests sometimes receive a normal JSON response. Buffer it
	// completely so a top-level error envelope cannot masquerade as HTTP 200.
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if fr.status < 300 && !strings.Contains(contentType, "text/event-stream") {
		fr.body, err = io.ReadAll(resp.Body)
		if san != nil {
			fr.body = san.restoreResponse(fr.body)
		}
		if hasLogicalErrorEnvelope(fr.body) {
			fr.status = logicalErrorStatus(fr.body)
			p.trace(r, "upstream.logical_error", map[string]any{"upstream_id": up.ID, "status": fr.status, "body": p.Debug.Body(fr.body, contentType)})
		} else {
			p.trace(r, "upstream.response_body", map[string]any{"upstream_id": up.ID, "status": fr.status, "body": p.Debug.Body(fr.body, contentType)})
		}
		if capture != nil {
			capture.Write(fr.body)
		}
		return fr, err
	}

	// For SSE, buffer the first complete data frame before committing. Error
	// frames can then use the normal alias and upstream failover path.
	reader := bufio.NewReader(resp.Body)
	first, readErr := readFirstSSEFrame(reader, 64<<10)
	if fr.status >= 300 {
		rest, _ := io.ReadAll(reader)
		first = append(first, rest...)
		fr.body = first
		p.trace(r, "upstream.response_body", map[string]any{"upstream_id": up.ID, "status": fr.status, "body": p.Debug.Body(first, resp.Header.Get("Content-Type"))})
		fr = p.autofixHTTPS(up, prefix, r, body, fr, doOnce)
		if capture != nil {
			capture.Write(fr.body)
		}
		return fr, nil
	}
	if hasLogicalErrorEnvelope(first) {
		rest, _ := io.ReadAll(reader)
		fr.body = append(first, rest...)
		fr.status = logicalErrorStatus(fr.body)
		p.trace(r, "upstream.logical_error", map[string]any{"upstream_id": up.ID, "status": fr.status, "body": p.Debug.Body(fr.body, contentType)})
		if capture != nil {
			capture.Write(fr.body)
		}
		return fr, nil
	}
	if readErr != nil {
		if readErr == io.EOF {
			return nil, fmt.Errorf("upstream stream ended before its first frame")
		}
		return nil, readErr
	}
	// Commit once. Every incomplete exit must bypass success revalidation;
	// cancellation may happen even when the last buffered read reports EOF.
	fr.committed = true
	defer func() {
		if r.Context().Err() != nil {
			fr.streamFailed = true
		}
	}()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	fl := w.(http.Flusher)
	streamLog := &debuglog.Stream{Logger: p.Debug, Emit: func(body any) {
		p.trace(r, "upstream.stream_chunk", map[string]any{"upstream_id": up.ID, "body": body})
	}}
	defer streamLog.Close()
	// Antigravity-backed routes: SSE is remapped through a line-buffered,
	// JSON-aware transformer (neutral tool names back to client names) while
	// keeping the existing flush/backpressure/cancellation behavior.
	rem := newSSERemapper(san)
	writeStream := func(b []byte) bool {
		if r.Context().Err() != nil {
			fr.streamFailed = true
			return false
		}
		if len(b) == 0 {
			return true
		}
		streamLog.Write(b)
		if capture != nil && capture.Len() < 2<<20 {
			capture.Write(b)
		}
		n, werr := w.Write(b)
		if werr == nil && n != len(b) {
			werr = io.ErrShortWrite
		}
		if werr != nil {
			fr.streamFailed = true
			p.trace(r, "client.write_error", map[string]any{"error": werr.Error()})
			return false
		}
		fl.Flush()
		return true
	}
	emit := func(raw []byte) bool { return writeStream(rem.transform(raw)) }
	flushTail := func() {
		if !fr.streamFailed {
			writeStream(rem.finish())
		}
	}
	if len(first) > 0 {
		if !emit(first) {
			fr.committed = true
			return fr, nil
		}
	}
	if readErr != nil {
		if readErr != io.EOF {
			fr.streamFailed = true
			p.trace(r, "upstream.read_error", map[string]any{"error": readErr.Error()})
		}
		flushTail()
		fr.committed = true
		return fr, nil
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			if !emit(buf[:n]) {
				break
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				fr.streamFailed = true
				p.trace(r, "upstream.read_error", map[string]any{"error": rerr.Error()})
			}
			break
		}
	}
	flushTail()
	fr.committed = true
	return fr, nil
}

func (p *Proxy) upstreamKeyOrder(up *config.Upstream, model string, ignoreCooldown bool) []string {
	keys := config.NormalizeAPIKeys(up.APIKeys)
	ready := func(key string) bool {
		if up.KeyModelBlocked(key, model) {
			return false
		}
		if !p.hitchanceEnabled() {
			return true
		}
		if ignoreCooldown {
			return !up.HitchanceQuarantined(key, model)
		}
		return up.HitchanceReady(key, model, time.Now())
	}
	if authMode(up) != "swap" || len(keys) == 0 {
		if !ready("") {
			return nil
		}
		return []string{""}
	}
	usable := keys[:0]
	for _, key := range keys {
		if ready(key) {
			usable = append(usable, key)
		}
	}
	keys = usable
	if len(keys) == 0 {
		return nil
	}
	p.upstreamKeyMu.Lock()
	defer p.upstreamKeyMu.Unlock()
	if !p.keyPoolSnapshotCurrent(up) {
		return nil
	}
	state := p.upstreamKeys[up.ID]
	if state == nil || state.identity != keyPoolIdentity(up) {
		state = &upstreamKeyPoolState{identity: keyPoolIdentity(up), cooldown: map[string]time.Time{}}
		p.upstreamKeys[up.ID] = state
	}
	start := int(state.cursor % uint64(len(keys)))
	state.cursor++
	now := time.Now()
	ordered := make([]string, 0, len(keys))
	for i := 0; i < len(keys); i++ {
		key := keys[(start+i)%len(keys)]
		if !p.hitchanceEnabled() {
			ordered = append(ordered, key)
			continue
		}
		until := state.cooldown[key]
		if ignoreCooldown || until.IsZero() || !until.After(now) {
			delete(state.cooldown, key)
			ordered = append(ordered, key)
			continue
		}
	}
	return ordered
}

func (p *Proxy) markUpstreamKeyFailure(up *config.Upstream, key string, status int, header http.Header) {
	if key == "" {
		return
	}
	duration := 5 * time.Minute
	if status == http.StatusTooManyRequests {
		duration = retryAfterCooldown(header)
	}
	p.upstreamKeyMu.Lock()
	defer p.upstreamKeyMu.Unlock()
	if !p.keyPoolSnapshotCurrent(up) {
		return
	}
	state := p.upstreamKeys[up.ID]
	if state == nil || state.identity != keyPoolIdentity(up) {
		state = &upstreamKeyPoolState{identity: keyPoolIdentity(up), cooldown: map[string]time.Time{}}
		p.upstreamKeys[up.ID] = state
	}
	state.cooldown[key] = time.Now().Add(duration)
}

func (p *Proxy) markUpstreamKeySuccess(up *config.Upstream, key string) {
	if key == "" {
		return
	}
	p.upstreamKeyMu.Lock()
	defer p.upstreamKeyMu.Unlock()
	if !p.keyPoolSnapshotCurrent(up) {
		return
	}
	if state := p.upstreamKeys[up.ID]; state != nil && state.identity == keyPoolIdentity(up) {
		delete(state.cooldown, key)
	}
}

// forwardOnce retries keys only when Hitchance classifies a scoped failure.
func (p *Proxy) forwardOnce(up *config.Upstream, typ, prefix string, r *http.Request, body []byte, capture *bytes.Buffer, w http.ResponseWriter) (*forwardResult, error) {
	model := modelFromBody(body)
	if model == "" {
		model = r.URL.Query().Get("model")
	}
	unavailable := func() *forwardResult {
		fr := conversionFailure(typ, 503, fmt.Errorf("no ready keys for this model (blocked or cooling down)"))
		fr.hitchance = hitchance.Decision{Action: "demote", Scope: "model", Category: "cooling"}
		return fr
	}
	// Refresh before every raw-model attempt, not only at candidate selection.
	up = p.currentHitchanceUpstream(up)
	if up == nil {
		return unavailable(), nil
	}
	ignoreCooldown := false
	if lastResort(r) && len(p.upstreamKeyOrder(up, model, false)) == 0 && !p.askedToWait(up, model) {
		// Everything here is cooling for a reason we may test: ask anyway, but
		// at most one probe per upstream and model family every probeInterval.
		ignoreCooldown = p.mayProbe(up, model)
	}
	keys := p.upstreamKeyOrder(up, model, ignoreCooldown)
	if len(keys) == 0 {
		return unavailable(), nil
	}
	var last *forwardResult
	for i, key := range keys {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		attempt := p.currentHitchanceUpstream(up)
		if attempt == nil {
			break
		}
		if !p.hitchanceKeyReady(attempt, key, model, ignoreCooldown) {
			continue
		}
		fr, err := p.forwardOnceWithKey(attempt, typ, prefix, r, body, capture, w, key)
		if err != nil {
			if r.Context().Err() == nil {
				p.observeUpstreamHealth(attempt, r, false)
				p.recordAttempt(attempt, key, false)
			}
			return nil, err
		}
		last = fr
		if err := r.Context().Err(); err != nil {
			if fr.committed {
				fr.streamFailed = true
				return fr, nil
			}
			return nil, err
		}
		if fr.localFailure {
			return fr, nil
		}
		if fr.committed && fr.streamFailed {
			p.recordAttempt(attempt, key, false)
			return fr, nil
		}
		if fr.status >= 200 && fr.status < 300 {
			p.observeUpstreamHealth(attempt, r, true)
			p.recordAttempt(attempt, key, true)
			if err := p.Cfg.ObserveHitchance(attempt, key, model, hitchance.Decision{}); err != nil {
				p.trace(r, "hitchance.persist_error", map[string]any{"upstream_id": up.ID})
			}
			p.markUpstreamKeySuccess(up, key)
			return fr, nil
		}
		in := hitchance.Input{Status: fr.status, Body: fr.body, Header: fr.header}
		if fr.upstreamEvidence != nil {
			in = *fr.upstreamEvidence
		}
		in.UpstreamID, in.Model = up.ID, model
		d := hitchance.Classify(p.Cfg.Get().Hitchance, in)
		// No stored key exists in delegated/no-auth contexts. Match the
		// persistence layer's non-destructive endpoint adaptation in metadata.
		if d.Action != "" && d.Action != "ignore" && (authMode(attempt) != "swap" || key == "") {
			if d.Scope == "key" {
				d.Scope = "endpoint"
			}
			if d.Action == "delete" {
				d.Action = "demote"
			}
		}
		if d.Action != "" {
			if d.Action != "ignore" {
				p.observeUpstreamHealth(attempt, r, false)
				p.recordAttempt(attempt, key, false)
			}
			fr.hitchance = d
			p.trace(r, "hitchance.decision", map[string]any{"upstream_id": up.ID, "key_fingerprint": config.KeyFingerprint(key), "model": model, "rule_id": d.RuleID, "category": d.Category, "requested_action": d.Action, "scope": d.Scope, "cooldown_seconds": int(d.Cooldown.Seconds())})
			if err := p.Cfg.ObserveHitchance(attempt, key, model, d); err != nil {
				p.trace(r, "hitchance.persist_error", map[string]any{"upstream_id": up.ID})
			}
			if d.Action != "ignore" && (d.Scope == "key" || d.Scope == "model") && authMode(up) == "swap" && i < len(keys)-1 {
				capture.Reset()
				continue
			}
			return fr, nil
		}
		return fr, nil
	}
	if last == nil {
		last = unavailable()
	}
	return last, nil
}

// autofixHTTPS retries a failed attempt over https when the upstream says
// plain HTTP was sent to an HTTPS port, and persists the fixed base_url
// (with the merge/delete https-twin logic). The marker is proof the port
// speaks TLS, so the URL is fixed even when the retry fails (e.g. cert not
// ignored yet) — the original response still goes to the client.
func (p *Proxy) autofixHTTPS(up *config.Upstream, prefix string, r *http.Request, body []byte, fr *forwardResult, doOnce func(string) (*http.Response, error)) *forwardResult {
	if fr.status < 400 || !p.Cfg.Get().ModelDiscovery.AutoFixProblems ||
		!strings.HasPrefix(strings.ToLower(up.BaseURL), "http://") ||
		!config.IsPlainHTTPOnHTTPS(string(fr.body)) {
		return fr
	}
	d := hitchance.Classify(p.Cfg.Get().Hitchance, hitchance.Input{Status: fr.status, Body: fr.body, Header: fr.header, UpstreamID: up.ID, Model: modelFromBody(body)})
	if r.Context().Err() != nil || d.Action == "" || d.Action == "ignore" {
		return fr
	}
	httpsBase := config.HTTPSBaseFor(up.BaseURL)
	if httpsBase == "" {
		return fr
	}
	httpsURL := upstreamURL(httpsBase, prefix, r.URL.Path)
	if r.URL.RawQuery != "" {
		httpsURL += "?" + r.URL.RawQuery
	}
	resp2, err := doOnce(httpsURL)
	if err != nil {
		p.trace(r, "upstream.error", map[string]any{"upstream_id": up.ID, "error": err.Error()})
		p.persistHTTPSFix(up.ID, httpsBase)
		log.Printf("[aisense] upstream %s auto-fixed http -> https (%s); retry failed: %v", up.ID, httpsBase, err)
		return fr
	}
	defer resp2.Body.Close()
	b2, readErr := io.ReadAll(resp2.Body)
	p.trace(r, "upstream.response", map[string]any{"upstream_id": up.ID, "status": resp2.StatusCode, "headers": p.Debug.Headers(resp2.Header), "body": p.Debug.Body(b2, resp2.Header.Get("Content-Type"))})
	if readErr != nil {
		p.trace(r, "upstream.read_error", map[string]any{"error": readErr.Error()})
	}
	if resp2.StatusCode >= 300 && config.IsPlainHTTPOnHTTPS(string(b2)) {
		return fr // https side also reports the marker — leave the URL as is
	}
	p.persistHTTPSFix(up.ID, httpsBase)
	log.Printf("[aisense] upstream %s auto-fixed http -> https (%s)", up.ID, httpsBase)
	fr.status = resp2.StatusCode
	fr.header = resp2.Header.Clone()
	fr.body = b2
	return fr
}

// persistHTTPSFix rewrites the upstream base_url to https, merging with an
// existing https twin (missing keys copied; identical keys drop the http
// upstream).
func (p *Proxy) persistHTTPSFix(upID, fixedBase string) {
	_ = p.Cfg.Update(func(c *config.Config) error {
		for _, u := range c.Upstreams {
			if u.ID == upID {
				u.BaseURL = fixedBase
				c.Upstreams, _, _ = config.MergeProtocolWinner(c.Upstreams, u)
				return nil
			}
		}
		return nil
	})
}

func shouldNormalizeUserAgent(req *http.Request) bool {
	ua := req.Header.Get("User-Agent")
	if ua == "" {
		return true
	}
	host := strings.ToLower(req.URL.Hostname())
	if host != "opencode.ai" {
		return false
	}
	bad := strings.ToLower(ua)
	return strings.Contains(bad, "go-http-client") || strings.Contains(bad, "python-urllib") ||
		strings.Contains(bad, "aisense")
}

func authMode(up *config.Upstream) string {
	if up.AuthMode != "" {
		return up.AuthMode
	}
	return "swap"
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func (p *Proxy) prefixFor(typ string) string {
	cfg := p.Cfg.Get()
	if typ == "anthropic" {
		if cfg.Server.Anthropic.Path != "" {
			return cfg.Server.Anthropic.Path
		}
		return "/v1"
	}
	if cfg.Server.OpenAI.Path != "" {
		return cfg.Server.OpenAI.Path
	}
	return "/v1"
}

func isStreaming(body []byte) bool {
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if v, ok := probe["stream"].(bool); ok && v {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// usage extraction

type usageData struct {
	In  int64
	Out int64
}

func extractUsage(typ string, buf []byte) usageData {
	var u usageData
	// 1) whole buffer is valid JSON (non-stream)
	if len(buf) > 0 && buf[0] == '{' {
		u2 := parseUsageJSON(typ, buf)
		if u2.In > 0 || u2.Out > 0 {
			return u2
		}
	}
	// 2) SSE: scan "data: {...}" lines, keep the last with usage
	lines := bytes.Split(buf, []byte("\n"))
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
		if len(payload) == 0 || payload[0] == '[' {
			continue
		}
		if u2 := parseUsageJSON(typ, payload); u2.In > 0 || u2.Out > 0 {
			if u2.In > 0 {
				u.In = u2.In
			}
			if u2.Out > 0 {
				u.Out = u2.Out
			}
		}
	}
	return u
}

func parseUsageJSON(typ string, b []byte) usageData {
	var u usageData
	switch typ {
	case "anthropic":
		var envelope struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(b, &envelope) == nil && len(envelope.Message) > 0 {
			return parseUsageJSON(typ, envelope.Message)
		}
		var m struct {
			Usage struct {
				InputTokens   int64 `json:"input_tokens"`
				OutputTokens  int64 `json:"output_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &m) == nil {
			u.In, u.Out = m.Usage.InputTokens+m.Usage.CacheRead+m.Usage.CacheCreation, m.Usage.OutputTokens
		}
	default:
		var m struct {
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &m) == nil {
			u.In = m.Usage.PromptTokens
			u.Out = m.Usage.CompletionTokens
			if u.In == 0 {
				u.In = m.Usage.InputTokens
			}
			if u.Out == 0 {
				u.Out = m.Usage.OutputTokens
			}
		}
	}
	return u
}

func modelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// effortFromBody extracts the client-requested reasoning effort. It
// understands the OpenAI chat/completions field (reasoning_effort), the
// responses API object (reasoning.effort), a generic top-level "effort", and
// Anthropic-style output_config.effort. Values like "none"/"default"/"auto"
// mean no preference.
func effortFromBody(body []byte) string {
	var m struct {
		ReasoningEffort any `json:"reasoning_effort"`
		Reasoning       any `json:"reasoning"`
		Effort          any `json:"effort"`
		OutputConfig    any `json:"output_config"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	effortString := func(v any) string {
		s, _ := v.(string)
		return s
	}
	normalize := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "", "none", "off", "disable", "disabled", "default", "auto", "null":
			return ""
		}
		if len(s) > 16 {
			return ""
		}
		return s
	}
	if e := normalize(effortString(m.ReasoningEffort)); e != "" {
		return e
	}
	if r, ok := m.Reasoning.(map[string]any); ok {
		if e := normalize(effortString(r["effort"])); e != "" {
			return e
		}
	}
	if e := normalize(effortString(m.Effort)); e != "" {
		return e
	}
	if oc, ok := m.OutputConfig.(map[string]any); ok {
		if e := normalize(effortString(oc["effort"])); e != "" {
			return e
		}
	}
	return ""
}

func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if upstreamModel == "" {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	cur, _ := m["model"].(string)
	if cur == "" || cur == upstreamModel {
		return body
	}
	m["model"] = upstreamModel
	next, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return next
}

// ---------------------------------------------------------------------------
// entry points

func (p *Proxy) ServeOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "openai", jsonErr)
}

func (p *Proxy) ServeAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "anthropic", anthropicErr)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, typ string, errFn func(http.ResponseWriter, int, string, string)) {
	body, bodyErr := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if r.Body != nil {
		r.Body.Close()
	}
	if p.Debug != nil {
		trace := &traceContext{id: newTraceID(), started: time.Now()}
		r = r.WithContext(context.WithValue(r.Context(), traceContextKey{}, trace))
		writer := &traceResponseWriter{ResponseWriter: w, proxy: p, req: r}
		w = writer
		p.trace(r, "request.received", map[string]any{
			"listener": typ, "method": r.Method, "url": p.Debug.URL(r.URL.String()),
			"headers": p.Debug.Headers(r.Header), "body": p.Debug.Body(body, r.Header.Get("Content-Type")),
		})
		defer func() {
			if writer.stream != nil {
				writer.stream.Close()
			}
			p.trace(r, "request.complete", map[string]any{
				"status": writer.status, "bytes_out": writer.bytes, "bytes_in": len(body),
				"attempts": trace.attempts, "selected_upstream": trace.selected,
				"duration_ms": time.Since(trace.started).Milliseconds(),
			})
		}()
	}
	if bodyErr != nil {
		p.trace(r, "request.read_error", map[string]any{"error": bodyErr.Error()})
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	key := p.findKey(r)
	if key == nil || !key.Enabled {
		errFn(w, 401, "authentication_error", "invalid api key")
		return
	}
	p.trace(r, "request.authenticated", map[string]any{"key_id": key.ID})

	model := modelFromBody(body)
	if model == "" {
		model = r.URL.Query().Get("model")
	}

	if isModelsListPath(r.URL.Path) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			errFn(w, http.StatusMethodNotAllowed, "invalid_request_error", "model listing endpoint only supports GET")
			return
		}
		p.writeModelsList(w, typ, key)
		p.touchKey(key)
		return
	}
	if modelID, ok := modelIDFromRetrievePath(r.URL.Path); ok {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			errFn(w, http.StatusMethodNotAllowed, "invalid_request_error", "model retrieve endpoint only supports GET")
			return
		}
		if p.writeModelObject(w, key, modelID) {
			p.touchKey(key)
			return
		}
		errFn(w, http.StatusNotFound, "not_found_error", "model "+modelID+" was not found")
		return
	}
	if isModelEndpointPath(r.URL.Path) {
		errFn(w, http.StatusMethodNotAllowed, "invalid_request_error", "model endpoint only supports GET")
		return
	}
	if model == "" {
		errFn(w, 400, "invalid_request_error", "model field is required")
		return
	}
	model = userModelName(model)
	if !allowedModel(key, model) && !anyAllowedModel(key, p.Cfg.Get().ComboModels(model)) {
		errFn(w, 403, "permission_error", "model "+model+" is not allowed for this api key")
		return
	}
	// A combo is a model name the user made up: walk its models in order and let
	// the next one take over as soon as no upstream can serve the current one.
	attempts := p.Cfg.Get().ComboModels(model)
	if len(attempts) == 0 {
		attempts = []string{model}
	} else {
		p.trace(r, "combo.expand", map[string]any{"combo": model, "models": attempts})
	}
	rateChecked := false
	summary := &attemptSummary{}
	policy := p.Cfg.Get().Hitchance
	cycles := policy.RetryCycles
	unlimited := policy.Enabled && cycles == 0
	if cycles < 1 {
		cycles = 1
	}
	budget := time.Duration(policy.CoolingRetrySeconds) * time.Second
	deadline := time.Now().Add(budget)
	if unlimited {
		deadline = time.Time{}
	}
	totalAttempts := 0
	rateLimited := map[string]bool{}
	everTried := false
	round := func(request *http.Request, cycle int) (answered, tried bool) {
		for _, attempt := range attempts {
			done, asked := p.serveModel(w, request, typ, errFn, key, body, attempt, cycle, &totalAttempts, rateLimited, &rateChecked, summary)
			tried = tried || asked
			if done {
				return true, tried
			}
		}
		return false, tried
	}
	for cycle := 1; unlimited || cycle <= cycles; cycle++ {
		if r.Context().Err() != nil {
			p.trace(r, "route.cancelled", map[string]any{"cycle": cycle, "attempts": totalAttempts})
			return
		}
		cycleLimit := any(cycles)
		if unlimited {
			cycleLimit = "infinite"
		}
		p.trace(r, "route.cycle_start", map[string]any{"cycle": cycle, "cycles": cycleLimit})
		answered, tried := round(r, cycle)
		if answered {
			return
		}
		everTried = everTried || tried
		// A non-rate-limit cooldown can be probed once if no upstream was
		// actually asked in this cycle. Provider rate limits are still honored.
		if !everTried && (unlimited || budget > 0) {
			var probeTried bool
			if answered, probeTried = round(withLastResort(r), cycle); answered {
				return
			}
			tried = tried || probeTried
			everTried = everTried || probeTried
			// If the request arrived during a short cooldown, waiting can
			// recover this same cycle without spending an extra real attempt.
			if !tried {
				if wait, ok := p.coolingWait(typ, attempts, deadline, false); ok {
					p.trace(r, "route.cooling_wait", map[string]any{"model": model, "wait_ms": wait.Milliseconds(), "rate_limit_only": false})
					select {
					case <-time.After(wait):
					case <-r.Context().Done():
						return
					}
					var resumedTried bool
					if answered, resumedTried = round(r, cycle); answered {
						return
					}
					everTried = everTried || resumedTried
				}
			}
		}
		p.trace(r, "route.cycle_complete", map[string]any{"cycle": cycle, "cycles": cycleLimit, "attempts": totalAttempts})
		if r.Context().Err() != nil {
			return
		}
		if unlimited || (cycle < cycles && budget > 0) {
			if wait, ok := p.coolingWait(typ, attempts, deadline, !unlimited && everTried); ok {
				p.trace(r, "route.cooling_wait", map[string]any{"model": model, "wait_ms": wait.Milliseconds(), "rate_limit_only": !unlimited && everTried})
				select {
				case <-time.After(wait):
				case <-r.Context().Done():
					return
				}
			} else if unlimited && !everTried {
				// No route has been tried and none is cooling down. There is
				// nothing to retry for this model under the current configuration.
				break
			} else if unlimited {
				// No cooling route can be scheduled (for example, all keys were
				// deleted). Recheck live configuration while the client still waits.
				select {
				case <-time.After(time.Second):
				case <-r.Context().Done():
					return
				}
			}
		}
	}
	p.trace(r, "route.retry_exhausted", map[string]any{"cycles": cycles, "attempts": totalAttempts})
	if reasons := summary.String(); reasons != "" {
		log.Printf("[aisense] %s unavailable%s", model, reasons)
		p.trace(r, "route.unavailable", map[string]any{"model": model, "reasons": strings.TrimSpace(reasons)})
	}
	errFn(w, 503, "unavailable", modelUnavailableMsg(model))
}

// coolingWait reports how long to wait for the first upstream of these models
// to come back before the retry deadline. With onlyRateLimits it waits only for rate limits.
func (p *Proxy) coolingWait(typ string, models []string, deadline time.Time, onlyRateLimits bool) (time.Duration, bool) {
	now := time.Now()
	if !deadline.IsZero() && !now.Before(deadline) {
		return 0, false
	}
	var soonest time.Time
	found := false
	for _, model := range models {
		for _, up := range p.eligibleCandidates(typ, model, true, nil) {
			until, ok := p.coolingUntil(up, model, onlyRateLimits)
			// Ready now but still unusable means something other than a cooldown
			// stands in the way — a blocked model, a filter — and no wait helps.
			if !ok || !until.After(now) {
				continue
			}
			if !found || until.Before(soonest) {
				soonest, found = until, true
			}
		}
	}
	if !found {
		return 0, false
	}
	wait := soonest.Sub(now) + 100*time.Millisecond
	if wait < 100*time.Millisecond {
		wait = 100 * time.Millisecond
	}
	if !deadline.IsZero() && now.Add(wait).After(deadline) {
		return 0, false
	}
	// Requests queued on the same rate limit should not all return at once and
	// trip it again: spread them over a quarter of a second.
	wait += mathrand.N(250 * time.Millisecond)
	if !deadline.IsZero() && now.Add(wait).After(deadline) {
		wait = deadline.Sub(now)
	}
	return wait, true
}

// serveModel routes one model name. It reports whether the client was answered
// — false only when no upstream could serve the model and nothing has been
// written yet, so a combo can move on to its next model — and whether any
// upstream was actually asked, which tells the caller apart from the case where
// every upstream was merely cooling down.
func (p *Proxy) serveModel(w http.ResponseWriter, r *http.Request, typ string, errFn func(http.ResponseWriter, int, string, string), key *config.APIKey, body []byte, model string, cycle int, totalAttempts *int, rateLimited map[string]bool, rateChecked *bool, summary *attemptSummary) (answered, tried bool) {
	if !allowedModel(key, model) {
		return false, tried // a combo never sends a model this API key may not use
	}
	requestedModel := model
	fuzzyTarget, fuzzy := p.fuzzyRoutingModel(typ, model)
	if fuzzy {
		if !allowedModel(key, fuzzyTarget) {
			errFn(w, 403, "permission_error", "model "+fuzzyTarget+" is not allowed for this api key")
			return true, tried
		}
		model = userModelName(fuzzyTarget)
	}
	filterFuzzy := func(candidates []*config.Upstream) []*config.Upstream {
		if !fuzzy {
			return candidates
		}
		out := candidates[:0]
		for _, up := range candidates {
			if len(fuzzyModelRoutes(up, key, requestedModel, fuzzyTarget)) > 0 {
				out = append(out, up)
			}
		}
		return out
	}
	// Reasoning effort (and the optional fast mode) may map onto specialized
	// upstream model variants (gpt-5.6-sol-low, …-fast, ...). The boost only
	// reorders candidate routes; the request body itself is still forwarded
	// with whatever model route wins, and authorization above still applies
	// to the model name the client actually asked for.
	boost := p.Cfg.Get().Models.VariantOptions().Suffixes(effortFromBody(body))
	permittedBoost := boost[:0]
	for _, suffix := range boost {
		if allowedModel(key, model+"-"+suffix) {
			permittedBoost = append(permittedBoost, suffix)
		}
	}
	boost = permittedBoost
	if fuzzy {
		boost = nil
	}
	cands := filterFuzzy(p.eligibleCandidates(typ, model, true, boost))
	if len(cands) == 0 {
		return false, tried
	}

	if !*rateChecked {
		if ok, lim := p.rateCheck(key); !ok {
			errFn(w, 429, "rate_limit_error", "rate limit exceeded ("+lim+")")
			return true, tried
		}
		*rateChecked = true
	}
	candidateIDs := make([]string, 0, len(cands))
	for _, candidate := range cands {
		candidateIDs = append(candidateIDs, candidate.ID)
	}
	p.trace(r, "route.candidates", map[string]any{"model": model, "upstreams": candidateIDs, "count": len(candidateIDs)})

	prefix := p.prefixFor(typ)
	cfg := p.Cfg.Get()
	var capture bytes.Buffer
	{
		cands = filterFuzzy(p.candidates(typ, model, true, boost))
		// A provider that just answered 429 is the last resort in this round.
		// Persisted rate-limit state gives the same order to the next request.
		limited := map[string]bool{}
		for _, up := range cands {
			limited[up.ID] = rateLimited[up.ID] || p.upstreamRateLimited(up)
		}
		sort.SliceStable(cands, func(i, j int) bool { return !limited[cands[i].ID] && limited[cands[j].ID] })
		for index, up := range cands {
			(*totalAttempts)++
			if trace := traceFrom(r); trace != nil {
				trace.attempts = *totalAttempts
				trace.selected = up.ID
			}
			release := p.Health.Begin(up)
			p.trace(r, "attempt.start", map[string]any{"cycle": cycle, "attempt": *totalAttempts, "total": len(cands), "upstream_id": up.ID})
			p.trace(r, "scheduler.select", map[string]any{
				"cycle": cycle, "attempt": *totalAttempts, "position": index + 1,
				"total": len(cands), "upstream_id": up.ID, "in_flight": p.Health.InFlight(up),
			})
			routes := p.combinedModelRoutes(up, model, boost)
			if fuzzy {
				routes = fuzzyModelRoutes(up, key, requestedModel, fuzzyTarget)
			}
			permitted := routes[:0]
			for _, route := range routes {
				if !upstreamModelBlocked(up, route) && allowedModel(key, route) && up.ModelKeyAvailable(route) {
					permitted = append(permitted, route)
				}
			}
			routes = permitted
			if len(routes) == 0 {
				release()
				continue
			}
			var fr *forwardResult
			var err error
			usedRoute := ""
			aliasFailure := false
			usedThinking := false
			for aliasIndex, upstreamModel := range routes {
				usedRoute = upstreamModel
				usedThinking = modelalias.IsThinking(upstreamModel)
				upstreamBody := rewriteModelInBody(body, upstreamModel)
				upstreamRequest := r
				if fuzzy && r.URL.Query().Get("model") != "" {
					upstreamRequest = r.Clone(r.Context())
					query := upstreamRequest.URL.Query()
					query.Set("model", upstreamModel)
					upstreamRequest.URL.RawQuery = query.Encode()
				}
				p.trace(r, "model_alias.attempt", map[string]any{
					"cycle": cycle, "upstream_id": up.ID, "requested_model": model,
					"upstream_model": upstreamModel, "alias_index": aliasIndex + 1, "alias_total": len(routes),
				})
				fr, err = p.forwardOnce(up, typ, prefix, upstreamRequest, upstreamBody, &capture, w)
				switch {
				case err != nil:
					tried = true
					summary.add(up.ID, "network error")
				case fr == nil:
				case fr.hitchance.Category == "cooling":
					summary.add(up.ID, "cooling")
				default:
					tried = true
					if fr.status < 200 || fr.status >= 300 {
						summary.add(up.ID, fr.hitchance.Category)
					}
				}
				if err != nil {
					break
				}
				if fr.committed || (fr.status >= 200 && fr.status < 300) {
					if fr.streamFailed {
						break
					}
					p.rememberModelRoute(up, model, usedThinking, boost, upstreamModel)
					p.rememberModelRoute(up, model, !usedThinking, boost, upstreamModel)
					break
				}
				aliasFailure = fr.hitchance.Scope == "model" && fr.hitchance.Action != "" && fr.hitchance.Action != "ignore"
				if aliasIndex+1 < len(routes) && aliasFailure {
					p.trace(r, "model_alias.retry", map[string]any{
						"cycle": cycle, "upstream_id": up.ID, "failed_model": upstreamModel,
						"status": fr.status, "next_model": routes[aliasIndex+1],
					})
					capture.Reset()
					continue
				}
				break
			}
			release()
			if err != nil {
				delete(rateLimited, up.ID)
			} else if fr != nil && !fr.localFailure && fr.hitchance.Category != "cooling" {
				rateLimited[up.ID] = fr.status == http.StatusTooManyRequests
			}
			if fr != nil && fr.committed && fr.streamFailed {
				p.Store.Add(key.ID, model, 0, 0, false)
				return true, tried // Never fail over once any client bytes have been committed.
			}
			if err != nil {
				if r.Context().Err() != nil {
					p.trace(r, "route.cancelled", map[string]any{"cycle": cycle, "attempt": *totalAttempts})
					return true, tried
				}
				d := hitchance.Classify(p.Cfg.Get().Hitchance, hitchance.Input{Status: 503, UpstreamID: up.ID, Model: model})
				if d.Action != "" && d.Action != "ignore" {
					d.Scope = "endpoint"
					p.Health.MarkFailure(up, model, routehealth.Global, d.Cooldown, "hitchance: "+d.Category)
					if d.Action == "delete" {
						d.Action = "demote"
					}
					if persistErr := p.Cfg.ObserveHitchance(up, "", model, d); persistErr != nil {
						p.trace(r, "hitchance.persist_error", map[string]any{"upstream_id": up.ID})
					}
				}
				p.trace(r, "attempt.error", map[string]any{"cycle": cycle, "attempt": *totalAttempts, "upstream_id": up.ID, "error": err.Error()})
				log.Printf("[aisense] upstream %s error: %v", up.ID, err)
				p.Store.Add(key.ID, model, 0, 0, false)
				if d.Action == "" || d.Action == "ignore" {
					errFn(w, 502, "upstream_error", "upstream request failed: "+err.Error())
					return true, tried
				}
				capture.Reset()
				continue
			}
			if fr.committed || (fr.status >= 200 && fr.status < 300) {
				p.Health.MarkSuccess(up, model)
				p.trace(r, "health.transition", map[string]any{"upstream_id": up.ID, "model": model, "status": "available"})
				p.trace(r, "attempt.complete", map[string]any{
					"cycle": cycle, "attempt": *totalAttempts, "upstream_id": up.ID,
					"status": fr.status, "streaming": fr.committed, "upstream_model": usedRoute,
				})
				if !fr.committed {
					copyHeader(w.Header(), fr.header)
					w.WriteHeader(fr.status)
					w.Write(fr.body)
				}
				if cfg.Usage.Enabled {
					u := extractUsage(typ, capture.Bytes())
					if !fr.committed {
						u = extractUsage(typ, fr.body)
					}
					p.Store.Add(key.ID, model, u.In, u.Out, true)
				} else {
					p.Store.Add(key.ID, model, 0, 0, true)
				}
				// Pin this working upstream for the model and track in used models for admin UI.
				p.recordUsedModel(model)
				p.setCachedUpstream(typ, model, up)
				p.touchKey(key)
				return true, tried
			}
			if fr.localFailure && fr.hitchance.Action == "" {
				copyHeader(w.Header(), fr.header)
				w.WriteHeader(fr.status)
				w.Write(fr.body)
				return true, tried
			}
			if fr.hitchance.Action != "" && fr.hitchance.Action != "ignore" {
				// Persisted key/exact-model decisions must not be widened to an
				// endpoint failure merely because this request exhausted its options.
				if !fr.localFailure && fr.hitchance.Scope == "endpoint" {
					p.Health.MarkFailure(up, model, routehealth.Global, fr.hitchance.Cooldown, "hitchance: "+fr.hitchance.Category)
				}
				if aliasFailure {
					p.forgetModelRoute(up, model)
				}
				p.Store.Add(key.ID, model, 0, 0, false)
				capture.Reset()
				continue
			}
			// A terminal response is never retried or cycled.
			copyHeader(w.Header(), fr.header)
			w.WriteHeader(fr.status)
			w.Write(fr.body)
			p.Store.Add(key.ID, model, 0, 0, false)
			p.touchKey(key)
			return true, tried
		}
		if r.Context().Err() != nil {
			return true, tried
		}
	}
	return false, tried
}

func (p *Proxy) touchKey(key *config.APIKey) {
	p.Cfg.Mutate(func(c *config.Config) {
		for _, k := range c.APIKeys {
			if k.ID == key.ID {
				k.LastUsed = time.Now()
			}
		}
	})
}
