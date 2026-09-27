package hitchance

import (
	"strings"
	"testing"
)

func TestHitchanceSSEOnlyErrorEvidence(t *testing.T) {
	for name, body := range map[string]string{
		"indented":   "  data: {\"error\":{\"message\":\"bad request\"},\"request\":{\"prompt\":\"invalid api key\"}}\n\n",
		"echo":       "data: {\"error\":{\"message\":\"bad request\"},\"request\":{\"prompt\":\"invalid api key\"}}\n\n",
		"multiline":  "event: error\ndata: {\"error\":{\"message\":\"bad request\"},\ndata: \"request\":{\"prompt\":\"invalid api key\"}}\n\n",
		"truncated":  "data: {\"error\":{\"message\":\"bad request\"},\"request\":{\"prompt\":\"invalid api key\"",
		"plain-data": "event: error\ndata: invalid api key\n\n",
		"success":    "data: {\"choices\":[{\"message\":{\"content\":\"invalid api key\"}}]}\n\n",
		"oversized":  "data: {\"error\":{\"message\":\"bad request\"},\"request\":{\"prompt\":\"invalid api key" + strings.Repeat("x", 64<<10) + "\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if text := ErrorText([]byte(body)); strings.Contains(text, "invalid api key") {
				t.Fatalf("echo/malformed SSE became evidence: %.150s", text)
			}
			if d := Classify(Default(), Input{Status: 400, Body: []byte(body)}); d.Action != "" {
				t.Fatalf("SSE echo classified: %+v", d)
			}
		})
	}
	body := []byte("event: error\ndata: {\"error\":{\"message\":\"invalid api key\"},\ndata: \"request\":{\"prompt\":\"bad request\"}}\n\n")
	if d := Classify(Default(), Input{Status: 401, Body: body}); d.Action != "delete" {
		t.Fatalf("real SSE error lost: %+v", d)
	}
}
