package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Only fixed categories leave this function. Never print log fields or bodies.
func analyzeLog(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	counts := map[string]int{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 64<<20)
	for s.Scan() {
		var record struct {
			Event  string `json:"event"`
			Status int    `json:"status"`
			Body   struct {
				Value json.RawMessage `json:"value"`
			} `json:"body"`
		}
		if json.Unmarshal(s.Bytes(), &record) != nil {
			counts["invalid_log_line"]++
			continue
		}
		if record.Event != "upstream.response_body" && record.Event != "upstream.logical_error" && record.Event != "upstream.response" {
			continue
		}
		b := record.Body.Value
		if len(b) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(b, &text) == nil {
			b = []byte(text)
		}
		status := record.Status
		if status == 0 {
			status = 200
		}
		_, reason := classify(status, b, "chat/completions")
		counts[reason]++
		var envelope map[string]json.RawMessage
		if json.Unmarshal(b, &envelope) == nil {
			for _, field := range []string{"error", "success", "choices", "content", "code"} {
				if raw, ok := envelope[field]; ok {
					kind := "other"
					if bytes.Equal(raw, []byte("false")) {
						kind = "false"
					}
					if bytes.Equal(raw, []byte("null")) {
						kind = "null"
					}
					counts["field:"+field+":"+kind]++
				}
			}
		}
		if reason == "logical_error_envelope" || reason == "non_json_response" || reason == "unexpected_sse" {
			lower := strings.ToLower(string(b))
			for _, marker := range []string{
				"data:", "<!doctype html", "<html", "invalid_request_error",
				"model", "provider", "unable to determine", "catalog",
				"not available", "not found", "unavailable", "unsupported",
				"key", "quota", "billing", "rate limit", "max_tokens", "max_completion_tokens",
			} {
				if strings.Contains(lower, marker) {
					counts[reason+":marker:"+marker]++
				}
			}
		}
	}
	if err := s.Err(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(counts)
}
