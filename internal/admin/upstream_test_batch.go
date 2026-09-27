package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/modelalias"
	"aisense/internal/protocol"
	"aisense/internal/proxy"
)

type upstreamTestRequest struct {
	IDs   []string `json:"ids,omitempty"`
	Tasks struct {
		DeleteInvalid  bool `json:"delete_invalid"`
		DisableInvalid bool `json:"disable_invalid"`
		HideInvalid    bool `json:"hide_invalid"`
	} `json:"tasks"`
	Checks struct {
		Models    bool `json:"models"`
		Responses bool `json:"responses"`
	} `json:"checks"`
	Confirm bool `json:"confirm"`
}

func (in upstreamTestRequest) validate() error {
	n := 0
	for _, v := range []bool{in.Tasks.DeleteInvalid, in.Tasks.DisableInvalid, in.Tasks.HideInvalid} {
		if v {
			n++
		}
	}
	if n > 1 {
		return errors.New("select at most one task; uncheck the active task first")
	}
	if (n > 0 || in.Checks.Responses) && !in.Confirm {
		return errors.New("confirmation is required for tasks or response checks")
	}
	return nil
}

type upstreamTestOutcome struct {
	up         *config.Upstream
	result     map[string]any
	invalid    bool
	removeKeys map[string]bool
	blocked    map[string][]string
	working    map[string][]string
}

func (s *Server) runUpstreamTests(w http.ResponseWriter, r *http.Request, stream bool) {
	var in upstreamTestRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": "invalid test request"})
		return
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		s.writeJSON(w, 400, map[string]string{"error": "unexpected trailing JSON"})
		return
	}
	if err := in.validate(); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !s.testMu.TryLock() {
		s.writeJSON(w, 409, map[string]string{"error": "another test batch is running"})
		return
	}
	defer s.testMu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	snapshot := s.Cfg.Get()
	ups := snapshotTestUpstreams(snapshot, in.IDs)
	flusher, canFlush := w.(http.Flusher)
	sse := strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if stream && !canFlush {
		s.writeJSON(w, 500, map[string]string{"error": "streaming is not supported"})
		return
	}
	if stream {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	emit := func(event any) bool {
		if !stream {
			return true
		}
		b, _ := json.Marshal(event)
		var err error
		if sse {
			_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		} else {
			_, err = fmt.Fprintf(w, "%s\n", b)
		}
		if err != nil {
			cancel()
			return false
		}
		flusher.Flush()
		return true
	}
	if !emit(map[string]any{"type": "start", "total": len(ups), "checks": in.Checks}) {
		return
	}
	transport := proxy.New(s.Cfg, nil)
	defer transport.CloseTestConnections()
	jobs := make(chan *config.Upstream)
	completed := make(chan upstreamTestOutcome, 8)
	progress := make(chan map[string]any, 8)
	var wg sync.WaitGroup
	for i := 0; i < min(8, len(ups)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for up := range jobs {
				if ctx.Err() != nil {
					return
				}
				out := s.checkUpstream(ctx, transport, up, in, func(detail map[string]any) {
					if !stream {
						return
					}
					select {
					case progress <- detail:
					case <-ctx.Done():
					}
				})
				select {
				case completed <- out:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, up := range ups {
			select {
			case jobs <- up:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(completed) }()
	// Wait for workers before releasing the batch lock or closing pooled clients.
	defer func() { cancel(); wg.Wait() }()
	var outcomes []upstreamTestOutcome
	results := []map[string]any{}
	okCount, unknownCount, invalidCount := 0, 0, 0
	requestCount := 0
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if !emit(map[string]any{"type": "heartbeat"}) {
				return
			}
		case detail := <-progress:
			if !emit(detail) {
				return
			}
		case out, open := <-completed:
			if !open {
				break loop
			}
			outcomes = append(outcomes, out)
			results = append(results, out.result)
			requestCount += intValue(out.result["requests"])
			if out.result["ok"] == true {
				okCount++
			} else if out.invalid {
				invalidCount++
			} else {
				unknownCount++
			}
			if !emit(map[string]any{"type": "result", "result": out.result, "completed": len(outcomes), "total": len(ups), "ok_count": okCount, "fail_count": invalidCount, "unknown_count": unknownCount}) {
				return
			}
		}
	}
	// Workers have exited, but their final progress can still be buffered.
drain:
	for {
		select {
		case detail := <-progress:
			if !emit(detail) {
				return
			}
		default:
			break drain
		}
	}
	changed, err := s.commitUpstreamTests(ctx, snapshot, in, outcomes)
	if err != nil {
		if stream {
			emit(map[string]any{"type": "error", "error": err.Error()})
		} else {
			s.writeJSON(w, 409, map[string]string{"error": err.Error()})
		}
		return
	}
	summary := map[string]any{"type": "complete", "total": len(ups), "completed": len(outcomes), "ok_count": okCount, "fail_count": invalidCount, "unknown_count": unknownCount, "changed": changed, "requests": requestCount, "checks": in.Checks}
	if stream {
		emit(summary)
	} else {
		summary["results"] = results
		s.writeJSON(w, 200, summary)
	}
}

func (s *Server) checkUpstream(ctx context.Context, transport *proxy.Proxy, up *config.Upstream, in upstreamTestRequest, progress func(map[string]any)) upstreamTestOutcome {
	start := time.Now()
	out := upstreamTestOutcome{up: up, removeKeys: map[string]bool{}, blocked: map[string][]string{}, working: map[string][]string{}}
	out.result = map[string]any{"id": up.ID, "ok": false, "valid": "unknown", "detail": "Not verified: see check details; ambiguous failures are retained.", "checks": in.Checks, "capabilities": map[string]protocol.Evidence{}}
	requests := 0
	diagnostics := []map[string]any{}
	defer func() {
		out.result["latency_ms"] = time.Since(start).Milliseconds()
		out.result["requests"] = requests
		out.result["diagnostics"] = diagnostics
	}()
	// One discovery budget across credentials, protocols, pages and confirmations.
	// Inference keeps its own per-request budget so large catalogs can be tested.
	discoveryBudget := 25 * time.Second
	discoveryExhausted := false
	keys := config.NormalizeAPIKeys(up.APIKeys)
	if len(keys) == 0 || up.AuthMode == "none" || up.AuthMode == "oauth" || up.AuthMode == "passthrough" {
		keys = []string{""}
	}
	if up.AuthMode == "passthrough" {
		out.result["detail"] = "Not verified: passthrough requires client credentials."
		return out
	}
	var resolver protocol.Resolver
	validKeys, invalidKeys := 0, 0
	outageSlots := 0
	endpointStopped := false
	out.result["key_total"] = len(keys)
	// stopOnOutage ends credential iteration once two consecutive slots saw
	// only endpoint-level failures and no slot produced positive evidence.
	// Untested credentials stay untested: no invalidity, no destructive edit.
	// It returns the number of untested remaining slots when stopping.
	stopOnOutage := func(keyIndex int) int {
		if endpointStopped || outageSlots < 2 || validKeys > 0 || len(out.working) > 0 || len(out.blocked) > 0 {
			return 0
		}
		remaining := len(keys) - keyIndex - 1
		if remaining <= 0 {
			return 0
		}
		endpointStopped = true
		out.result["endpoint_unavailable"] = true
		out.result["untested_keys"] = remaining
		return remaining
	}
	for keyIndex, key := range keys {
		if ctx.Err() != nil {
			return out
		}
		discoveryStart := time.Now()
		discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, discoveryBudget)
		caps := map[string]protocol.Evidence{}
		lastRequest := "no request made"
		report := func(check, state, reason string, modelIndex, modelTotal int) {
			detail := map[string]any{"type": "progress", "id": up.ID, "check": check, "key_index": keyIndex + 1, "key_total": len(keys), "model_index": modelIndex, "model_total": modelTotal, "valid": state, "reason": reason, "requests": requests}
			// Bound retained diagnostics; streaming still reports every model.
			if len(diagnostics) < 200 {
				diagnostics = append(diagnostics, detail)
			} else {
				out.result["diagnostics_truncated"] = true
			}
			progress(detail)
		}
		send := func(c context.Context, op string, body []byte) (*http.Response, error) {
			copy := *up
			catalog := op == "models" || strings.HasPrefix(op, "models?")
			if catalog && !caps[protocol.Chat].Supported && caps[protocol.Messages].Supported {
				copy.Type = "anthropic"
			}
			request := func() (*http.Response, error) {
				if c.Err() != nil {
					lastRequest = "deadline exceeded or cancelled before request"
					return nil, c.Err()
				}
				requests++
				resp, err := transport.TestRequest(c, &copy, key, op, body)
				lastRequest = "transport failure"
				if errors.Is(err, context.DeadlineExceeded) {
					lastRequest = "request deadline exceeded"
				} else if errors.Is(err, context.Canceled) {
					lastRequest = "request cancelled"
				} else if err == nil {
					lastRequest = fmt.Sprintf("HTTP %d", resp.StatusCode)
				}
				return resp, err
			}
			resp, err := request()
			if catalog && err == nil && (resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 405) {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				if copy.Type == "anthropic" {
					copy.Type = "openai"
				} else {
					copy.Type = "anthropic"
				}
				previous := lastRequest
				resp, err = request()
				lastRequest = previous + "; alternate auth: " + lastRequest
				return resp, err
			}
			return resp, err
		}
		identity := config.KeyFingerprint(key) + up.ID
		for _, op := range []string{protocol.Chat, protocol.Messages} {
			caps[op] = resolver.Probe(discoveryCtx, identity, op, send)
			e := caps[op]
			state := "unknown"
			if e.Supported {
				state = "schema only"
			} else if e.Unsupported {
				state = "unsupported"
			}
			report("capability "+op, state, fmt.Sprintf("%s (HTTP %d); authentication and inference not verified", e.Source, e.Status), 0, 0)
		}
		out.result["capabilities"] = caps
		slotOutage := endpointWideOutage(caps)
		if !in.Checks.Models && !in.Checks.Responses {
			discoveryExhausted = discoveryExhausted || discoveryCtx.Err() != nil
			discoveryBudget -= time.Since(discoveryStart)
			cancelDiscovery()
			if caps[protocol.Chat].Supported || caps[protocol.Messages].Supported {
				validKeys++
			}
			if slotOutage {
				outageSlots++
			} else {
				outageSlots = 0
			}
			if remaining := stopOnOutage(keyIndex); remaining > 0 {
				report("endpoint", "unknown", fmt.Sprintf("endpoint unavailable: repeated transport/5xx failures across %d credential slots; %d remaining credential slots not tested; no destructive action", outageSlots, remaining), 0, 0)
				break
			}
			continue // Lack of capability evidence never proves invalidity.
		}
		models, catalogState, catalogReason := testCatalog(discoveryCtx, send)
		if catalogState == "invalid" {
			models, catalogState, catalogReason = testCatalog(discoveryCtx, send)
		}
		report("catalog", catalogState, catalogReason+"; "+lastRequest, 0, len(models))
		// A definitive catalog result overrides the outage classification.
		if slotOutage && catalogState != "unknown" {
			slotOutage = false
		}
		discoveryExhausted = discoveryExhausted || discoveryCtx.Err() != nil
		discoveryBudget -= time.Since(discoveryStart)
		cancelDiscovery()
		if slotOutage {
			outageSlots++
		} else {
			outageSlots = 0
		}
		if remaining := stopOnOutage(keyIndex); remaining > 0 {
			report("endpoint", "unknown", fmt.Sprintf("endpoint unavailable: repeated transport/5xx failures across %d credential slots; %d remaining credential slots not tested; no destructive action", outageSlots, remaining), 0, 0)
			break
		}
		if in.Checks.Models && !in.Checks.Responses {
			if catalogState == "valid" {
				validKeys++
			} else if catalogState == "invalid" {
				invalidKeys++
			}
			continue
		}
		if catalogState != "valid" {
			models = nil
			for _, model := range up.Models {
				raw := up.ModelAliases[model]
				if len(raw) == 0 {
					raw = []string{model}
				}
				models = append(models, raw...)
			}
		}
		models = normalizeStringList(models)
		sort.Strings(models)
		var operations []string
		for _, op := range []string{protocol.Chat, protocol.Messages} {
			if caps[op].Supported {
				operations = append(operations, op)
			}
		}
		if len(operations) == 0 && !caps[protocol.Chat].Unsupported {
			// Match the resolver's conservative native fallback without re-probing.
			// Expired discovery must not suppress configured-model response checks.
			operations = append(operations, protocol.Chat)
		}
		good, bad, skipped, unknown := 0, 0, 0, 0
		for modelIndex, raw := range models {
			if ctx.Err() != nil {
				return out
			}
			state := "unknown"
			reason := "no compatible operation available within discovery budget"
			if raw == "*" || modelalias.IsMetaName(raw) || nonChatModel(raw) {
				state = "skipped"
				reason = "non-chat or meta model"
			} else if len(operations) > 0 {
				state = "invalid"
				for _, op := range operations {
					attempt, attemptReason := testModelResponse(ctx, send, op, raw)
					// A second independent failure on every supported protocol is
					// required before persisting a route block.
					if attempt == "invalid" {
						if caps[op].Supported {
							report("response "+op, "unconfirmed", attemptReason+"; "+lastRequest+"; confirmation required", modelIndex+1, len(models))
							attempt, attemptReason = testModelResponse(ctx, send, op, raw)
						} else {
							attempt = "unknown"
							attemptReason = "model failure on unconfirmed protocol; retained"
						}
					}
					reason = attemptReason + "; " + lastRequest
					report("response "+op, attempt, reason, modelIndex+1, len(models))
					if attempt == "valid" {
						state = "valid"
						break
					}
					if attempt == "unknown" || state == "unknown" {
						state = "unknown"
					} else if attempt == "skipped" {
						state = "skipped"
					}
				}
			}
			switch state {
			case "valid":
				good++
				out.working[config.KeyFingerprint(key)] = append(out.working[config.KeyFingerprint(key)], raw)
			case "invalid":
				bad++
				out.blocked[config.KeyFingerprint(key)] = append(out.blocked[config.KeyFingerprint(key)], raw)
			case "skipped":
				skipped++
			default:
				unknown++
			}
			// Never send response bodies, credentials, URLs or provider error messages.
			report("model summary", state, reason, modelIndex+1, len(models))
		}
		if good > 0 {
			validKeys++
		}
		if bad > 0 && good == 0 && skipped == 0 && unknown == 0 && catalogState == "valid" {
			invalidKeys++
			out.removeKeys[key] = true
		} else if good == 0 && catalogState == "invalid" && in.Checks.Models {
			invalidKeys++
		}
		out.result["tested"] = intValue(out.result["tested"]) + good + bad + unknown
		out.result["skipped"] = intValue(out.result["skipped"]) + skipped
		out.result["blocked"] = intValue(out.result["blocked"]) + bad
		out.result["succeeded"] = intValue(out.result["succeeded"]) + good
		out.result["unknown"] = intValue(out.result["unknown"]) + unknown
	}
	out.result["valid_keys"] = validKeys
	out.result["invalid_keys"] = invalidKeys
	out.invalid = invalidKeys == len(keys)
	if validKeys > 0 {
		out.result["ok"], out.result["valid"] = true, "yes"
		out.result["detail"] = "Valid catalog evidence only; inference and model availability were not verified. Unconfirmed keys are retained."
		if in.Checks.Responses {
			out.result["detail"] = "Valid response evidence for at least one model/key pair, not proof every pair works. See check counts; unconfirmed models and keys are retained."
		}
		if !in.Checks.Models && !in.Checks.Responses {
			out.result["detail"] = "Valid capability evidence only; authentication and model availability were not verified. No inference or config edits."
		}
	} else if out.invalid {
		out.result["valid"] = "no"
		out.result["detail"] = "Confirmed invalid: every credential returned an empty catalog or all advertised supported models repeatedly failed."
	} else if len(out.blocked) > 0 {
		out.result["detail"] = "Not verified as a whole. Unknown models and keys are retained; individually confirmed model failures are blocked."
	}
	if endpointStopped {
		out.result["detail"] = fmt.Sprint(out.result["detail"]) + " Endpoint unavailable: repeated transport/5xx failures across credential slots; untested keys are retained."
	}
	if discoveryExhausted {
		out.result["detail"] = fmt.Sprint(out.result["detail"]) + " Discovery budget exhausted or cancelled; remaining discovery checks are unverified."
	}
	return out
}

// endpointWideOutage reports whether one credential slot saw only
// endpoint-level failures: transport breakdowns or gateway 502/503/504 on
// every probed operation. Authentication, rate limits, other statuses, and
// any positive evidence never qualify; definitive catalog results are
// handled by the caller. Such outages are endpoint properties, not per-key
// evidence, so they never mark credentials invalid.
func endpointWideOutage(caps map[string]protocol.Evidence) bool {
	for _, op := range []string{protocol.Chat, protocol.Messages} {
		e := caps[op]
		switch {
		case e.Supported:
			return false
		case e.Source == "transport_failure":
		case e.Source == "server_error" && (e.Status == 502 || e.Status == 503 || e.Status == 504):
		default:
			return false
		}
	}
	return true
}

func intValue(v any) int { n, _ := v.(int); return n }

func testCatalog(ctx context.Context, send protocol.Send) ([]string, string, string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var models []string
	op := "models"
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		resp, err := send(ctx, op, nil)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
				return nil, "unknown", "catalog deadline exceeded; retained"
			}
			return nil, "unknown", "catalog request failed; retained"
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if ctx.Err() != nil {
			return nil, "unknown", "catalog deadline exceeded or cancelled while reading; retained"
		}
		if err != nil || len(b) > 4<<20 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, "unknown", "catalog HTTP/read/size failure; retained"
		}
		var list struct {
			upstreamModelList
			HasMore    bool            `json:"has_more"`
			LastID     string          `json:"last_id"`
			Error      json.RawMessage `json:"error"`
			Total      int             `json:"total"`
			TotalCount int             `json:"total_count"`
		}
		if json.Unmarshal(b, &list) != nil || list.Data == nil || len(list.Error) > 0 && string(list.Error) != "null" {
			return nil, "unknown", "not a model catalog (invalid JSON, missing data, or error envelope)"
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(b, &fields)
		for _, name := range []string{"next", "next_page", "next_cursor", "next_page_token", "continuation_token", "pagination"} {
			value := string(fields[name])
			if value != "" && value != "null" && value != `""` && value != "false" {
				return nil, "unknown", "unsupported pagination; incomplete catalog"
			}
		}
		for _, item := range list.Data {
			id, ok := item["id"].(string)
			if !ok || strings.TrimSpace(id) == "" || id == "*" || modelalias.IsMetaName(id) {
				return nil, "unknown", "invalid model identifier in catalog"
			}
		}
		models = append(models, normalizeDiscoveredModels(list.upstreamModelList)...)
		if len(models) > 100000 {
			return nil, "unknown", "catalog model limit exceeded"
		}
		if !list.HasMore {
			if list.Total > len(models) || list.TotalCount > len(models) {
				return nil, "unknown", "catalog count indicates missing models"
			}
			if len(models) == 0 {
				return nil, "invalid", "complete empty catalog"
			}
			return normalizeStringList(models), "valid", "nonempty complete catalog; inference not verified"
		}
		if list.LastID == "" || seen[list.LastID] {
			return nil, "unknown", "missing or repeated pagination cursor"
		}
		seen[list.LastID] = true
		op = "models?after_id=" + url.QueryEscape(list.LastID)
	}
	return nil, "unknown", "catalog page limit exceeded"
}

// cannedGatewayPatterns are signatures of fake "gateway" services that reply
// to every prompt with a fixed template instead of model output. Matched
// case-insensitively against successful response bodies during response
// checks; real models answering an ordinary test prompt do not produce these.
var cannedGatewayPatterns = []string{
	"successfully processed prompt",
	"gateway status: operational",
	"9router gateway",
}

func cannedGatewayResponse(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, pattern := range cannedGatewayPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func nonChatModel(raw string) bool {
	s := strings.ToLower(raw)
	for _, token := range []string{"embedding", "embed-", "rerank", "whisper", "tts", "dall-e", "image", "moderation", "transcri", "stable-diffusion", "flux", "sora", "video"} {
		if strings.Contains(s, token) {
			return true
		}
	}
	return false
}

func testModelResponse(ctx context.Context, send protocol.Send, op, raw string) (string, string) {
	body, _ := json.Marshal(map[string]any{"model": raw, "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 16, "stream": false})
	if op == protocol.Messages {
		var err error
		body, err = protocol.Request(body, "openai")
		if err != nil {
			return "skipped", "request conversion unsupported"
		}
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := send(c, op, body)
	if err != nil {
		return "unknown", "response request failed; retained"
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if c.Err() != nil {
		return "unknown", "response deadline exceeded or cancelled while reading; retained"
	}
	if err != nil || len(b) > 1<<20 {
		return "unknown", "response body unreadable or too large; retained"
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Fake gateways answer every prompt with a canned template instead of
		// running a model. Such responses are structured, reproducible
		// failures, not generated output.
		if cannedGatewayResponse(b) {
			return "invalid", "canned gateway response without generation"
		}
		if protocol.Successful(op, b) {
			return "valid", "nonempty operation-specific response envelope"
		}
	}
	// Status alone is never proof. In particular auth, rate, server, HTML and
	// generic protocol errors cannot remove a credential or model.
	if resp.StatusCode == 405 || resp.StatusCode == 501 {
		return "skipped", "operation not supported"
	}
	if resp.StatusCode != 400 && resp.StatusCode != 404 && resp.StatusCode != 410 && resp.StatusCode != 422 {
		return "unknown", "HTTP failure or unrecognized success envelope; retained"
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &envelope) != nil {
		return "unknown", "unrecognized error envelope; retained"
	}
	message := strings.ToLower(envelope.Error.Message)
	for _, token := range []string{"not a chat", "not supported on this endpoint", "embedding", "rerank", "image generation", "audio", "video", "unsupported modality"} {
		if strings.Contains(message, token) {
			return "skipped", "unsupported model modality"
		}
	}
	for _, token := range []string{"permission", "access", "auth", "rate", "quota", "billing", "temporar", "timeout", "overload"} {
		if strings.Contains(message, token) {
			return "unknown", "permission, quota or transient failure; retained"
		}
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Error.Type
	}
	if code == "unsupported_model_type" || code == "unsupported_operation" || code == "model_not_supported" {
		return "skipped", "unsupported model or operation"
	}
	switch code {
	case "model_not_found", "model_retired":
		return "invalid", "explicit model_not_found or model_retired"
	}
	return "unknown", "ambiguous provider error; retained"
}

func (s *Server) commitUpstreamTests(ctx context.Context, snapshot *config.Config, in upstreamTestRequest, outcomes []upstreamTestOutcome) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	changes := map[string]*config.Upstream{}
	for _, out := range outcomes {
		up := *out.up
		up.KeyBlockedModels = config.CloneModelAliases(up.KeyBlockedModels)
		remove := false
		if in.Checks.Responses {
			for hash, models := range out.working {
				good := map[string]bool{}
				for _, raw := range models {
					good[raw] = true
				}
				var remaining []string
				for _, raw := range up.KeyBlockedModels[hash] {
					if !good[raw] {
						remaining = append(remaining, raw)
					}
				}
				if len(remaining) == 0 {
					delete(up.KeyBlockedModels, hash)
				} else {
					up.KeyBlockedModels[hash] = remaining
				}
			}
			if up.KeyBlockedModels == nil && len(out.blocked) > 0 {
				up.KeyBlockedModels = map[string][]string{}
			}
			for hash, models := range out.blocked {
				up.KeyBlockedModels[hash] = normalizeStringList(append(up.KeyBlockedModels[hash], models...))
			}
			if len(out.removeKeys) > 0 {
				keys := []string{}
				for _, key := range up.APIKeys {
					if !out.removeKeys[key] {
						keys = append(keys, key)
					} else {
						delete(up.KeyBlockedModels, config.KeyFingerprint(key))
					}
				}
				up.APIKeys = keys
				remove = len(keys) == 0 || out.removeKeys[""]
			}
		}
		if out.invalid {
			if in.Tasks.DeleteInvalid {
				remove = true
			}
			if in.Tasks.DisableInvalid {
				up.Enabled = false
			}
			if in.Tasks.HideInvalid {
				up.HiddenInvalid = true
			}
		}
		if remove {
			changes[up.ID] = nil
		} else if !reflect.DeepEqual(&up, out.up) {
			changes[up.ID] = &up
		}
	}
	if len(changes) == 0 {
		return 0, nil
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Reject the whole plan when any tested input/transport changed; unrelated
		// admin/API-key settings are left intact in the current transaction.
		if !reflect.DeepEqual(s.Cfg.Get().Upstreams, snapshot.Upstreams) || !reflect.DeepEqual(c.Proxies, snapshot.Proxies) || !reflect.DeepEqual(c.ModelDiscovery, snapshot.ModelDiscovery) {
			return errors.New("configuration changed during testing; no test changes were applied, run again")
		}
		remaining := make([]*config.Upstream, 0, len(c.Upstreams))
		for _, up := range c.Upstreams {
			if changed, ok := changes[up.ID]; ok {
				if changed != nil {
					remaining = append(remaining, changed)
				}
			} else {
				remaining = append(remaining, up)
			}
		}
		c.Upstreams = remaining
		return ctx.Err()
	})
	if err != nil {
		return 0, err
	}
	return len(changes), nil
}
