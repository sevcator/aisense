package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"aisense/internal/config"
	"aisense/internal/protocol"
)

func probeCapabilities(ctx context.Context, up *config.Upstream, client *http.Client) map[string]protocol.Evidence {
	result := map[string]protocol.Evidence{}
	if up.AuthMode == "oauth" || up.AuthMode == "passthrough" {
		for _, op := range []string{protocol.Chat, protocol.Messages} {
			result[op] = protocol.Evidence{Source: "requires_proxy_auth"}
		}
		return result
	}
	keys := config.NormalizeAPIKeys(up.APIKeys)
	if len(keys) == 0 {
		keys = []string{""}
	}
	if len(keys) > 2 {
		keys = keys[:2]
	}
	for _, op := range []string{protocol.Chat, protocol.Messages} {
		for _, key := range keys {
			identity, _ := json.Marshal([]any{up.BaseURL, up.AuthMode, up.OAuth, up.APIKeys, key, "admin"})
			e := protocol.Shared.Probe(ctx, string(identity), op, func(ctx context.Context, op string, body []byte) (*http.Response, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, protocol.Endpoint(up.BaseURL, op), bytes.NewReader(body))
				if err != nil {
					return nil, err
				}
				copy := *up
				copy.Type = protocol.Format(op)
				applyModelDiscoveryAuth(req, &copy, key)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("User-Agent", "OpenAI/Python 1.99.0")
				return client.Do(req)
			})
			result[op] = e
			if e.Supported || e.Unsupported || ctx.Err() != nil {
				break
			}
		}
	}
	return result
}
