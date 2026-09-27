package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/protocol"
)

func (p *Proxy) forwardOnceWithKey(up *config.Upstream, typ, prefix string, r *http.Request, body []byte, capture *bytes.Buffer, w http.ResponseWriter, key string) (*forwardResult, error) {
	op := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	// Antigravity account-ban protection: when the per-attempt route targets
	// an Antigravity-backed model (ag/ or antigravity/ prefix), sanitize the
	// outgoing payload (system/developer stripping + neutral tool names),
	// scrub client-identifying headers, and map tool names back in the
	// response. Chat/messages operations only; embeddings and other
	// operations are untouched.
	san, route := p.antigravitySanitizer(op, r, body)
	if san != nil {
		body = san.sanitizeRequest(body)
		p.trace(r, "antigravity.sanitize", map[string]any{
			"route": route, "op": op,
			"system_stripped": san.SystemStripped, "tools_renamed": san.ToolsRenamed,
		})
	}
	identityBytes, _ := json.Marshal([]any{up.BaseURL, up.APIKeys, up.AuthMode, up.OAuth, up.UseProxy, p.Cfg.Get().Proxies, p.Cfg.Get().ModelDiscovery.IgnoreCertErrors, key, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")})
	identity := string(identityBytes)
	send := func(ctx context.Context, operation string, b []byte) (*http.Response, error) {
		req, err := p.buildUpstreamRequest(up, protocol.Format(operation), r.WithContext(ctx), b, protocol.Endpoint(up.BaseURL, operation), key, san)
		if err != nil {
			return nil, err
		}
		req.Method = http.MethodPost
		req.Header.Set("Content-Type", "application/json")
		req.Header.Del("Content-Encoding")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Del("Anthropic-Beta")
		return p.doWithResponseStartTimeout(req, up.UseProxy)
	}
	// The client's native request is itself the strongest capability check and
	// avoids speculative probes on working native endpoints. Only uncommitted
	// protocol/auth failures need invalid-body discovery.
	other := protocol.Messages
	if op == protocol.Messages {
		other = protocol.Chat
	}
	e, known := protocol.Shared.Cached(identity, op)
	alt, altKnown := protocol.Shared.Cached(identity, other)
	var native *forwardResult
	if e.Supported || !known && !altKnown || !e.Unsupported && !alt.Supported {
		var err error
		native, err = p.forwardNative(up, typ, prefix, r, body, capture, w, key, san)
		if err != nil {
			return nil, err
		}
		if native.committed || native.status >= 200 && native.status < 300 {
			evidence := native.body
			if native.committed && capture != nil {
				evidence = capture.Bytes()
			}
			if native.status < 300 && protocol.Successful(op, evidence) {
				protocol.Shared.Remember(identity, op)
			}
			return native, nil
		}
		decision := hitchance.Classify(p.Cfg.Get().Hitchance, hitchance.Input{Status: native.status, Body: native.body, Header: native.header, UpstreamID: up.ID, Model: modelFromBody(body)})
		// Only a bare status (no recognised error text) can mean the endpoint speaks
		// the other protocol; a matched error text is the provider's real answer.
		if decision.Action == "" || decision.Action == "ignore" || decision.TextMatched || decision.Scope != "key" && decision.Scope != "endpoint" {
			return native, nil
		}
		if modelalias.RetryableError(native.status, string(native.body)) || bytes.Contains(native.body, []byte("data:")) {
			return native, nil
		}
		if native.status != 400 && native.status != 401 && native.status != 403 && native.status != 404 && native.status != 405 && native.status != 501 {
			return native, nil
		}
	}
	dest, err := protocol.Shared.Resolve(r.Context(), identity, op, send)
	if err != nil {
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		return conversionFailure(typ, 400, err), nil
	}
	if dest == op {
		if native != nil {
			return native, nil
		}
		fr, err := p.forwardNative(up, typ, prefix, r, body, capture, w, key, san)
		if err == nil && fr.status >= 200 && fr.status < 300 {
			evidence := fr.body
			if fr.committed && capture != nil {
				evidence = capture.Bytes()
			}
			if protocol.Successful(op, evidence) {
				protocol.Shared.Remember(identity, op)
			}
		}
		return fr, err
	}
	if r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("Content-Encoding") != "" {
		return conversionFailure(typ, 400, fmt.Errorf("unsupported cross-format beta or encoded request")), nil
	}
	for name := range r.URL.Query() {
		if name != "model" {
			return conversionFailure(typ, 400, fmt.Errorf("unsupported cross-format query parameter %s", name)), nil
		}
	}
	if modelFromBody(body) == "" && r.URL.Query().Get("model") != "" {
		var value map[string]any
		if json.Unmarshal(body, &value) == nil && value != nil {
			value["model"] = r.URL.Query().Get("model")
			body, _ = json.Marshal(value)
		}
	}
	converted, err := protocol.Request(body, typ)
	if err != nil {
		return conversionFailure(typ, 400, err), nil
	}
	// Conversion is content-preserving, so the converted payload inherits the
	// sanitization of the input; this second idempotent pass guarantees the
	// payload that is actually sent carries no system content or client tool
	// names.
	converted = san.sanitizeRequest(converted)
	if capture != nil {
		capture.Reset()
	}
	resp, err := send(r.Context(), dest, converted)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	fr := &forwardResult{status: resp.StatusCode, header: resp.Header.Clone()}
	for k := range fr.header {
		if hopHeaders[k] {
			fr.header.Del(k)
		}
	}
	for _, k := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding", "ETag", "Content-MD5", "Digest"} {
		fr.header.Del(k)
	}
	fr.header.Set("Content-Type", "application/json")
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return conversionFailure(typ, 502, fmt.Errorf("unsupported encoded cross-format response")), nil
	}
	if resp.StatusCode >= 300 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
		if err != nil {
			return nil, err
		}
		if len(b) > 32<<20 {
			return conversionFailure(typ, 502, fmt.Errorf("converted response exceeds 32 MiB")), nil
		}
		if hasLogicalErrorEnvelope(b) && fr.status < 300 {
			fr.status = logicalErrorStatus(b)
		}
		if fr.status >= 300 {
			fr.upstreamEvidence = &hitchance.Input{Status: fr.status, Body: b, Header: resp.Header.Clone()}
			fr.body = protocol.Error(b, typ)
		} else {
			fr.body, err = protocol.Response(b, protocol.Format(dest))
			if err != nil {
				return conversionFailure(typ, 502, err), nil
			}
			if san != nil {
				fr.body = san.restoreResponse(fr.body)
			}
			protocol.Shared.Remember(identity, dest)
		}
		if capture != nil {
			capture.Write(fr.body)
		}
		return fr, nil
	}
	stream := &protocol.Stream{From: protocol.Format(dest)}
	rem := newSSERemapper(san)
	reader := bufio.NewReader(resp.Body)
	for {
		frame, readErr := protocol.ReadFrame(reader)
		var output []byte
		if readErr == nil {
			output, err = stream.Feed(frame)
			if err == nil && len(output) > 0 {
				output = rem.transform(output)
			}
		} else if readErr == io.EOF && stream.Complete() {
			return fr, nil
		} else {
			err = readErr
			if err == io.EOF {
				err = fmt.Errorf("upstream stream ended before completion")
			}
		}
		if err != nil {
			if !fr.committed {
				if r.Context().Err() != nil {
					return nil, r.Context().Err()
				}
				status := 502
				if hasLogicalErrorEnvelope(frame) {
					status = logicalErrorStatus(frame)
					// This is an upstream error, not a converter rejection. Keep
					// its error fields as evidence even though its wire format changes.
					b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": hitchance.ErrorText(frame)}})
					return &forwardResult{status: status, header: fr.header, body: protocol.Error(b, typ),
						upstreamEvidence: &hitchance.Input{Status: status, Body: frame, Header: resp.Header.Clone()}}, nil
				}
				return nil, err
			}
			fr.streamFailed = true
			if r.Context().Err() == nil {
				_, _ = w.Write(stream.Failure(err))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return fr, nil
		}
		if len(output) == 0 {
			continue
		}
		if !fr.committed {
			fr.header.Set("Content-Type", "text/event-stream")
			copyHeader(w.Header(), fr.header)
			w.WriteHeader(fr.status)
			fr.committed = true
		}
		if capture != nil {
			if stream.Complete() && capture.Len() >= 2<<20 {
				capture.Reset()
			}
			if capture.Len() < 2<<20 {
				capture.Write(output)
			}
		}
		if _, err = w.Write(output); err != nil {
			fr.streamFailed = true
			return fr, nil
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if stream.Complete() {
			protocol.Shared.Remember(identity, dest)
			return fr, nil
		}
	}
}

func conversionFailure(typ string, status int, err error) *forwardResult {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error()}})
	return &forwardResult{localFailure: true, status: status, header: http.Header{"Content-Type": []string{"application/json"}}, body: protocol.Error(b, typ)}
}
