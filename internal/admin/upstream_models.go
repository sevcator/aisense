package admin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"aisense/internal/autodiscovery"
	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/modelalias"
	"aisense/internal/protocol"
)

type upstreamModelList struct {
	Data []map[string]any `json:"data"`
}

type modelRefreshResult struct {
	capabilities   map[string]protocol.Evidence
	validEmpty     bool
	models         []string
	modelAliases   map[string][]string
	err            error
	selectedBase   string
	protocolChange string
	keyCount       int
}

type modelsRefreshSummary struct {
	AutoDiscoveryError string                   `json:"auto_discovery_error,omitempty"`
	AutoDiscoveryNames int                      `json:"auto_discovery_names,omitempty"`
	Updated            int                      `json:"updated"`
	Results            []map[string]interface{} `json:"results"`
}

// discoveryClient builds the model-discovery HTTP client. When
// IgnoreCertErrors is set the TLS layer skips certificate verification.
func discoveryClient(md config.ModelDiscoveryCfg, timeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if md.IgnoreCertErrors {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: protocol.NoRedirect}
}

type discoveryProgress struct {
	Running   bool   `json:"running"`
	Queued    int    `json:"queued"`
	AllQueued bool   `json:"all_queued"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Updated   int    `json:"updated"`
	Failed    int    `json:"failed"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) queueModelDiscovery(ids []string) {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()
	if s.discoveryWake == nil {
		s.discoveryWake = make(chan struct{}, 1)
	}
	if s.discoveryPending == nil {
		s.discoveryPending = map[string]bool{}
	}
	if len(ids) == 0 {
		s.discoveryAll = true
	}
	for _, id := range ids {
		if id != "" {
			s.discoveryPending[id] = true
		}
	}
	select {
	case s.discoveryWake <- struct{}{}:
	default:
	}
}

func (s *Server) StartModelDiscovery(ctx context.Context) {
	s.discoveryMu.Lock()
	if s.discoveryStarted {
		s.discoveryMu.Unlock()
		return
	}
	s.discoveryStarted = true
	if s.discoveryWake == nil {
		s.discoveryWake = make(chan struct{}, 1)
	}
	wake := s.discoveryWake
	s.discoveryMu.Unlock()
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		scan := time.NewTicker(30 * time.Second)
		defer scan.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cfg := s.Cfg.Get()
				if cfg.ModelDiscovery.Enabled || cfg.AutoModelsDiscovery {
					s.queueModelDiscovery(nil)
				}
				s.queueUndiscoveredUpstreams()
				timer.Reset(modelDiscoveryInterval(cfg.ModelDiscovery))
			case <-scan.C:
				// Pick up upstreams imported while running (file tools, watcher
				// reloads) without waiting for the periodic discovery interval.
				s.queueUndiscoveredUpstreams()
			case <-wake:
				s.discoveryMu.Lock()
				var ids []string
				for id := range s.discoveryPending {
					ids = append(ids, id)
				}
				all := s.discoveryAll
				s.discoveryAll, s.discoveryPending = false, nil
				s.discoveryMu.Unlock()
				// Do not let an auto-routes-only sweep swallow full catalog imports.
				if all && len(ids) > 0 {
					s.queueModelDiscovery(nil)
					all = false
				}
				if all {
					ids = nil
				}
				if all || len(ids) > 0 {
					_, _ = s.refreshUpstreamModels(ctx, ids)
				}
			}
		}
	}()
}

func modelDiscoveryInterval(cfg config.ModelDiscoveryCfg) time.Duration {
	minutes := cfg.RefreshIntervalMinutes
	if minutes <= 0 {
		minutes = 60
	}
	if minutes < 1 {
		minutes = 1
	}
	return time.Duration(minutes) * time.Minute
}

// queueUndiscoveredUpstreams queues catalog discovery for enabled upstreams
// that have never been discovered (models_refreshed_at unset). This runs
// regardless of the periodic-refresh toggles so freshly imported upstreams
// get their model catalogs without operator action. Upstreams whose discovery
// failed record a timestamp too, so the scan never retries them on its own.
func (s *Server) queueUndiscoveredUpstreams() {
	var ids []string
	for _, up := range s.Cfg.Get().Upstreams {
		if up != nil && up.Enabled && up.ModelsRefreshedAt.IsZero() {
			ids = append(ids, up.ID)
		}
	}
	if len(ids) > 0 {
		s.queueModelDiscovery(ids)
	}
}

func (s *Server) refreshUpstreamModels(ctx context.Context, ids []string) (modelsRefreshSummary, error) {
	s.discoveryRun.Lock()
	defer s.discoveryRun.Unlock()
	s.discoveryMu.Lock()
	s.discoveryStatus = discoveryProgress{Running: true}
	s.discoveryMu.Unlock()
	defer func() { s.discoveryMu.Lock(); s.discoveryStatus.Running = false; s.discoveryMu.Unlock() }()
	summary := modelsRefreshSummary{Results: []map[string]interface{}{}}
	selected := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			selected[id] = true
		}
	}
	cfg := s.Cfg.Get()
	// Explicit import/save IDs always discover the ordinary catalog. The auto
	// routes toggle only narrows periodic/manual all-upstream refreshes.
	autoOnly := len(ids) == 0 && cfg.AutoModelsDiscovery && !cfg.ModelDiscovery.Enabled
	// Protocol-twin merging compares every upstream against every other, so the
	// identities are computed once per run. Recomputing them inside the pair
	// loop made large catalogs stall for seconds before discovery even started.
	twinIdentities := map[string]string{}
	if cfg.ModelDiscovery.AutoFixProblems && !autoOnly {
		for _, twin := range cfg.Upstreams {
			if twin != nil {
				twinIdentities[twin.ID] = config.ProtocolEndpointIdentity(twin)
			}
		}
	}
	var autoNames []string
	if cfg.AutoModelsDiscovery {
		s.mu.Lock()
		if s.autoDiscovery == nil {
			s.autoDiscovery = &autodiscovery.Cache{}
		}
		source := s.autoDiscovery
		s.mu.Unlock()
		var err error
		autoNames, err = source.Refresh(ctx)
		summary.AutoDiscoveryNames = len(autoNames)
		if err != nil {
			summary.AutoDiscoveryError = err.Error()
			log.Printf("[auto-discovery] source refresh failed (cached names=%d): %v", len(autoNames), err)
			if autoOnly && len(autoNames) == 0 {
				s.discoveryMu.Lock()
				s.discoveryStatus.Error = "Auto-route source unavailable"
				s.discoveryMu.Unlock()
				return summary, err
			}
			s.discoveryMu.Lock()
			s.discoveryStatus.Error = "Auto-route source unavailable; continuing with cached source or ordinary catalogs"
			s.discoveryMu.Unlock()
		}
	}
	upstreams := []*config.Upstream{}
	for _, up := range cfg.Upstreams {
		if up == nil || up.BaseURL == "" {
			continue
		}
		if !up.Enabled {
			continue
		}
		if len(selected) > 0 && !selected[up.ID] {
			continue
		}
		copy := *up
		copy.APIKeys = append([]string(nil), up.APIKeys...)
		copy.Models = append([]string(nil), up.Models...)
		copy.ModelAliases = config.CloneModelAliases(up.ModelAliases)
		copy.BlockedModels = append([]string(nil), up.BlockedModels...)
		copy.FailCodes = append([]int(nil), up.FailCodes...)
		if cfg.ModelDiscovery.AutoFixProblems && !autoOnly {
			identity := twinIdentities[up.ID]
			for _, twin := range cfg.Upstreams {
				if twin != nil && twin.Enabled && (len(selected) == 0 || selected[twin.ID]) && twin.ID != up.ID && twinIdentities[twin.ID] == identity {
					config.MergeUpstreamKeys(&copy, twin)
				}
			}
		}
		upstreams = append(upstreams, &copy)
	}
	if len(upstreams) == 0 {
		return summary, nil
	}
	s.discoveryMu.Lock()
	s.discoveryStatus.Total = len(upstreams)
	s.discoveryMu.Unlock()
	// Keep protocol twins in one job so automatic merging sees both outcomes.
	groups := map[string][]*config.Upstream{}
	for _, up := range upstreams {
		identity := config.ExactEndpointIdentity(up)
		if cfg.ModelDiscovery.AutoFixProblems && !autoOnly {
			identity = config.ProtocolEndpointIdentity(up)
		}
		groups[identity] = append(groups[identity], up)
	}
	jobs := make(chan []*config.Upstream, len(groups))
	for _, group := range groups {
		jobs <- group
	}
	close(jobs)
	type completed struct {
		up  *config.Upstream
		res modelRefreshResult
	}
	done := make(chan []completed, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				if ctx.Err() != nil {
					return
				}
				var batch []completed
				for _, up := range group {
					md := cfg.ModelDiscovery
					if autoOnly {
						md.AutoFixProblems = false
					}
					res := discoverUpstreamModels(ctx, up, md, s.Debug)
					if autoOnly && res.validEmpty {
						res.err = nil
					}
					if autoOnly && res.err == nil {
						res = autoOnlyResult(up, res, autoNames)
					}
					batch = append(batch, completed{up, res})
				}
				done <- batch
			}
		}()
	}
	go func() { workers.Wait(); close(done) }()
	results := map[string]modelRefreshResult{}
	var errs []error
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	commit := func() {
		if len(results) == 0 {
			return
		}
		if ctx.Err() != nil {
			results = map[string]modelRefreshResult{}
			return
		}
		updated, err := s.commitModelResults(cfg, autoOnly, results)
		if err != nil {
			errs = append(errs, err)
			s.discoveryMu.Lock()
			s.discoveryStatus.Error = "Could not save discovery results"
			s.discoveryMu.Unlock()
		}
		summary.Updated += updated
		s.discoveryMu.Lock()
		s.discoveryStatus.Updated = summary.Updated
		s.discoveryMu.Unlock()
		results = map[string]modelRefreshResult{}
	}
	for {
		select {
		case batch, ok := <-done:
			if !ok {
				commit()
				return summary, errors.Join(errs...)
			}
			for _, item := range batch {
				up, res := item.up, item.res
				results[up.ID] = res
				detail := map[string]interface{}{
					"capabilities": res.capabilities,
					"id":           up.ID, "base_url": up.BaseURL, "selected_base_url": res.selectedBase,
					"selected_protocol": strings.ToLower(strings.SplitN(res.selectedBase, ":", 2)[0]),
					"protocol_change":   res.protocolChange, "key_count": res.keyCount,
				}
				if res.err != nil {
					detail["error"] = res.err.Error()
				}
				summary.Results = append(summary.Results, detail)
				if res.err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", up.ID, res.err))
				}
				s.discoveryMu.Lock()
				s.discoveryStatus.Completed++
				if res.err != nil {
					s.discoveryStatus.Failed++
				}
				s.discoveryMu.Unlock()
			}
			if len(results) >= 16 {
				commit()
			}
		case <-ticker.C:
			commit()
		}
	}
}

func (s *Server) commitModelResults(cfg *config.Config, autoOnly bool, results map[string]modelRefreshResult) (int, error) {
	originals := map[string]*config.Upstream{}
	for _, up := range cfg.Upstreams {
		if up != nil {
			originals[up.ID] = up
		}
	}
	updated := 0
	now := time.Now().UTC()
	if err := s.Cfg.Update(func(c *config.Config) error {
		live := map[string]*config.Upstream{}
		for _, up := range s.Cfg.Get().Upstreams {
			if up != nil {
				live[up.ID] = up
			}
		}
		staleGroups := map[string]bool{}
		if cfg.ModelDiscovery.AutoFixProblems && !autoOnly {
			for id := range results {
				if !reflect.DeepEqual(originals[id], live[id]) && originals[id] != nil {
					staleGroups[config.ProtocolEndpointIdentity(originals[id])] = true
				}
			}
		}
		byID := map[string]*config.Upstream{}
		for _, up := range c.Upstreams {
			if up != nil {
				byID[up.ID] = up
			}
		}

		// First record failures. They remain intact unless a verified winner in
		// their protocol group is selected below.
		for id, res := range results {
			if up := byID[id]; up != nil {
				if !up.Enabled || !reflect.DeepEqual(originals[id], live[id]) || staleGroups[config.ProtocolEndpointIdentity(up)] || !reflect.DeepEqual(cfg.ModelDiscovery, c.ModelDiscovery) || cfg.AutoModelsDiscovery != c.AutoModelsDiscovery {
					delete(results, id)
					continue
				}
				up.ModelsRefreshedAt = now
				if res.err != nil {
					up.ModelsRefreshError = res.err.Error()
				}
			}
		}

		groups := map[string][]string{}
		for id := range results {
			if up := byID[id]; up != nil {
				identity := config.ExactEndpointIdentity(up)
				if c.ModelDiscovery.AutoFixProblems && !autoOnly {
					identity = config.ProtocolEndpointIdentity(up)
				}
				groups[identity] = append(groups[identity], id)
			}
		}
		for identity, ids := range groups {
			if identity == "" {
				continue
			}
			var winner *config.Upstream
			var winnerRes modelRefreshResult
			for _, id := range ids {
				res := results[id]
				candidate := byID[id]
				if candidate == nil || res.err != nil {
					continue
				}
				candidateScheme := strings.ToLower(strings.SplitN(candidate.BaseURL, ":", 2)[0])
				selectedScheme := strings.ToLower(strings.SplitN(res.selectedBase, ":", 2)[0])
				score := 0
				if selectedScheme == "https" {
					score += 2
				}
				if candidateScheme == selectedScheme {
					score++
				}
				winnerScore := -1
				if winner != nil {
					winnerScheme := strings.ToLower(strings.SplitN(winnerRes.selectedBase, ":", 2)[0])
					winnerScore = 0
					if winnerScheme == "https" {
						winnerScore += 2
					}
					if strings.HasPrefix(strings.ToLower(winner.BaseURL), winnerScheme+"://") {
						winnerScore++
					}
				}
				if winner == nil || score > winnerScore {
					winner, winnerRes = candidate, res
				}
			}
			if winner == nil {
				continue // neither protocol worked: keep every row
			}
			// Never merge/delete an unselected, disabled, newly added or edited twin.
			mergeSafe := true
			for _, twin := range c.Upstreams {
				if twin != nil && config.ProtocolEndpointIdentity(twin) == config.ProtocolEndpointIdentity(winner) {
					if _, ok := results[twin.ID]; !ok || !reflect.DeepEqual(originals[twin.ID], live[twin.ID]) {
						mergeSafe = false
					}
				}
			}
			winner.BaseURL = winnerRes.selectedBase
			mergedAliases := map[string][]string{}
			for _, id := range ids {
				res := results[id]
				if res.err != nil || !strings.EqualFold(strings.SplitN(res.selectedBase, ":", 2)[0], strings.SplitN(winnerRes.selectedBase, ":", 2)[0]) {
					continue
				}
				for _, model := range res.models {
					mergedAliases[model] = append(mergedAliases[model], res.modelAliases[model]...)
				}
			}
			catalog := modelalias.Build(nil, mergedAliases)
			winner.Models = catalog.Models
			winner.ModelAliases = catalog.Aliases
			winner.ModelsRefreshError = ""
			winner.ModelsRefreshedAt = now
			updated++
			if c.ModelDiscovery.AutoFixProblems && !autoOnly && mergeSafe {
				c.Upstreams, _, _ = config.MergeProtocolWinner(c.Upstreams, winner)
				byID = map[string]*config.Upstream{}
				for _, up := range c.Upstreams {
					if up != nil {
						byID[up.ID] = up
					}
				}
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return updated, nil
}

// Refresh only recognized auto routes; leave the ordinary catalog untouched.
// Raw aliases, not display names, are the evidence of upstream support.
func autoOnlyResult(up *config.Upstream, res modelRefreshResult, names []string) modelRefreshResult {
	var routes []string
	for _, model := range up.Models {
		aliases := up.ModelAliases[model]
		if len(aliases) == 0 {
			aliases = []string{model}
		}
		for _, raw := range aliases {
			if !autodiscovery.Matches(names, raw) {
				routes = append(routes, raw)
			}
		}
	}
	for _, model := range res.models {
		aliases := res.modelAliases[model]
		if len(aliases) == 0 {
			aliases = []string{model}
		}
		for _, raw := range aliases {
			if autodiscovery.Matches(names, raw) {
				routes = append(routes, raw)
			}
		}
	}
	catalog := modelalias.Build(routes, nil)
	res.models, res.modelAliases = catalog.Models, catalog.Aliases
	return res
}

func dbgEvent(dbg *debuglog.Logger, event string, fields map[string]any) {
	if dbg == nil {
		return
	}
	dbg.Event("admin", event, fields)
}

func discoverUpstreamModels(ctx context.Context, up *config.Upstream, md config.ModelDiscoveryCfg, dbg *debuglog.Logger) modelRefreshResult {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if up.UseProxy || up.AuthMode == "oauth" || up.AuthMode == "passthrough" {
		return modelRefreshResult{err: errors.New("catalog discovery requires runtime proxy or delegated credentials; existing catalog retained")}
	}
	base := config.NormalizeBaseURL(up.BaseURL)
	primary := discoverModelsAtBase(ctx, up, base, md, dbg)
	primary.selectedBase = base
	if !md.AutoFixProblems {
		return primary
	}
	opposite := config.OppositeProtocolBase(base)
	if opposite == "" {
		return primary
	}
	// A working HTTPS endpoint is already the preferred outcome. A working
	// HTTP endpoint still probes HTTPS so both-working endpoints converge to
	// HTTPS; a failed primary always probes its opposite.
	if primary.err == nil && strings.HasPrefix(strings.ToLower(base), "https://") {
		return primary
	}
	alternate := discoverModelsAtBase(ctx, up, opposite, md, dbg)
	alternate.selectedBase = opposite
	if alternate.err == nil && (primary.err != nil || strings.HasPrefix(strings.ToLower(opposite), "https://")) {
		alternate.protocolChange = strings.ToUpper(strings.SplitN(base, ":", 2)[0]) + " → " + strings.ToUpper(strings.SplitN(opposite, ":", 2)[0])
		return alternate
	}
	return primary
}

func discoverModelsAtBase(ctx context.Context, up *config.Upstream, base string, md config.ModelDiscoveryCfg, dbg *debuglog.Logger) modelRefreshResult {
	keys := config.NormalizeAPIKeys(up.APIKeys)
	keyCount := len(keys)
	if len(keys) == 0 {
		keys = []string{""}
	}
	client := discoveryClient(md, 20*time.Second)
	defer client.CloseIdleConnections()
	probeUp := *up
	probeUp.BaseURL = base
	probeCtx, probeCancel := context.WithTimeout(ctx, 12*time.Second)
	capabilities := probeCapabilities(probeCtx, &probeUp, client)
	probeCancel()
	// Use confirmed operation evidence, not the persisted hint, for credentials.
	if capabilities[protocol.Chat].Supported {
		probeUp.Type = "openai"
	} else if capabilities[protocol.Messages].Supported {
		probeUp.Type = "anthropic"
	}
	seen := map[string]bool{}
	models := []string{}
	failures := []string{}
	validEmpty := false
	for _, key := range keys {
		if ctx.Err() != nil {
			failures = append(failures, "endpoint discovery deadline exceeded or cancelled")
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, protocol.Endpoint(base, "models"), nil)
		if err != nil {
			cancel()
			failures = append(failures, "invalid models URL")
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "OpenAI/Python 1.99.0")
		applyModelDiscoveryAuth(req, &probeUp, key)
		dbgEvent(dbg, "discovery.request", map[string]any{"upstream_id": up.ID, "method": req.Method, "url": dbg.URL(req.URL.String()), "headers": dbg.Headers(req.Header)})
		resp, doErr := client.Do(req)
		if doErr == nil && (resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 405) {
			// A models endpoint may use different authentication from its chat
			// endpoints. Retry credentials, but never infer operation support.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			alternate := probeUp
			if alternate.Type == "anthropic" {
				alternate.Type = "openai"
			} else {
				alternate.Type = "anthropic"
			}
			req = req.Clone(attemptCtx)
			req.Header.Del("Authorization")
			req.Header.Del("X-Api-Key")
			req.Header.Del("Anthropic-Version")
			applyModelDiscoveryAuth(req, &alternate, key)
			resp, doErr = client.Do(req)
		}
		if doErr != nil {
			dbgEvent(dbg, "discovery.error", map[string]any{"upstream_id": up.ID, "method": req.Method, "url": dbg.URL(req.URL.String()), "error": doErr.Error()})
			cancel()
			failure := "models request transport failure"
			if errors.Is(doErr, context.DeadlineExceeded) || errors.Is(doErr, context.Canceled) {
				failure = "models request deadline exceeded or cancelled"
			}
			failures = append(failures, failure)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		cancel()
		dbgEvent(dbg, "discovery.response", map[string]any{"upstream_id": up.ID, "status": resp.StatusCode, "headers": dbg.Headers(resp.Header), "body": dbg.Body(body, resp.Header.Get("Content-Type"))})
		if readErr != nil {
			failures = append(failures, "models response read failed")
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failures = append(failures, fmt.Sprintf("HTTP %d", resp.StatusCode))
			continue
		}
		var list upstreamModelList
		if err := json.Unmarshal(body, &list); err != nil {
			failures = append(failures, "invalid models response")
			continue
		}
		if list.Data != nil && len(list.Data) == 0 {
			validEmpty = true
		}
		for _, model := range normalizeDiscoveredModels(list) {
			if !seen[model] {
				seen[model] = true
				models = append(models, model)
			}
		}
	}
	sort.Strings(models)
	catalog := modelalias.Build(models, nil)
	result := modelRefreshResult{capabilities: capabilities, models: catalog.Models, modelAliases: catalog.Aliases, selectedBase: base, keyCount: keyCount}
	result.validEmpty = validEmpty && len(models) == 0 && len(failures) == 0
	if len(models) == 0 {
		detail := "no key returned a valid model list"
		if len(failures) > 0 {
			detail += ": " + strings.Join(failures, "; ")
		}
		result.err = errors.New(detail)
	}
	return result
}

func applyModelDiscoveryAuth(req *http.Request, up *config.Upstream, key string) {
	if up.Type == "anthropic" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if up.AuthMode == "none" || up.AuthMode == "oauth" || up.AuthMode == "passthrough" {
		return
	}
	if key == "" {
		return
	}
	if up.Type == "anthropic" {
		req.Header.Set("x-api-key", key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func normalizeDiscoveredModels(list upstreamModelList) []string {
	seen := map[string]bool{}
	models := []string{}
	for _, item := range list.Data {
		id, _ := item["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	sort.Strings(models)
	return models
}
