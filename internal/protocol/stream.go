package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ReadFrame bounds an individual SSE event, including comments and multiline data.
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	var b bytes.Buffer
	for b.Len() <= 1<<20 {
		line, prefix, err := r.ReadLine()
		if err != nil {
			if b.Len() > 0 && err == io.EOF {
				return b.Bytes(), nil
			}
			return nil, err
		}
		b.Write(line)
		if !prefix {
			b.WriteByte('\n')
			if len(line) == 0 {
				return b.Bytes(), nil
			}
		}
	}
	return nil, fmt.Errorf("SSE frame exceeds 1 MiB")
}
func event(name string, v any) []byte {
	b, _ := json.Marshal(v)
	prefix := ""
	if name != "" {
		prefix = "event: " + name + "\n"
	}
	return []byte(prefix + "data: " + string(b) + "\n\n")
}

type Stream struct {
	From              string
	started, finished bool
	id, model, reason string
	input, output     float64
	next              int
	textIndex         *int
	tools             map[int]int
	blocks            map[int]string
	args              map[int]bool
	cachedInput       float64
}

func (s *Stream) chunk(delta object, finish any, u any) []byte {
	m := object{"id": s.id, "object": "chat.completion.chunk", "created": 0, "model": s.model, "choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish}}}
	if u != nil {
		m["usage"] = u
	}
	return event("", m)
}
func (s *Stream) Failure(err error) []byte {
	m := object{"error": object{"type": "api_error", "message": err.Error()}}
	if s.From == "openai" {
		m["type"] = "error"
		return event("error", m)
	}
	return event("", m)
}
func (s *Stream) Complete() bool { return s.finished }

// Feed emits incremental text and tool JSON fragments without buffering a whole
// completion. Only IDs/indices and usage are retained across frames.
func (s *Stream) Feed(frame []byte) ([]byte, error) {
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 {
		return nil, nil
	}
	raw := strings.Join(data, "\n")
	if s.finished {
		return nil, fmt.Errorf("data after stream completion")
	}
	if s.tools == nil {
		s.tools = map[int]int{}
		s.blocks = map[int]string{}
		s.args = map[int]bool{}
	}
	if s.next > 1024 || len(s.blocks) > 1024 {
		return nil, unsupported("more than 1024 stream blocks")
	}
	if raw == "[DONE]" {
		if s.From != "openai" || !s.started || s.reason == "" {
			return nil, fmt.Errorf("premature stream completion")
		}
		var out []byte
		indices := []int{}
		for i := range s.blocks {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			out = append(out, event("content_block_stop", object{"type": "content_block_stop", "index": i})...)
		}
		out = append(out, event("message_delta", object{"type": "message_delta", "delta": object{"stop_reason": s.reason, "stop_sequence": nil}, "usage": object{"input_tokens": s.input, "output_tokens": s.output}})...)
		out = append(out, event("message_stop", object{"type": "message_stop"})...)
		s.finished = true
		return out, nil
	}
	var m object
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("invalid SSE JSON: %w", err)
	}
	if m["error"] != nil || m["type"] == "error" {
		msg := str(obj(m["error"])["message"])
		if msg == "" {
			msg = "upstream stream error"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if s.From == "anthropic" {
		switch str(m["type"]) {
		case "ping":
			return nil, nil
		case "message_start":
			if s.started {
				return nil, fmt.Errorf("duplicate message_start")
			}
			a := obj(m["message"])
			if len(arr(a["content"])) > 0 {
				return nil, unsupported("nonempty initial streamed content")
			}
			s.id = str(a["id"])
			s.model = str(a["model"])
			u := obj(a["usage"])
			s.cachedInput = number(u["cache_read_input_tokens"]) + number(u["cache_creation_input_tokens"])
			s.input = number(u["input_tokens"]) + s.cachedInput
			s.output = number(u["output_tokens"])
			s.started = true
			return s.chunk(object{"role": "assistant", "content": ""}, nil, nil), nil
		case "content_block_start":
			if !s.started {
				return nil, fmt.Errorf("content before message_start")
			}
			i := int(number(m["index"]))
			c := obj(m["content_block"])
			if _, ok := s.blocks[i]; ok || i < 0 || i >= 1024 {
				return nil, unsupported("invalid stream block index")
			}
			switch str(c["type"]) {
			case "text":
				if err := fields(c, "type text"); err != nil {
					return nil, err
				}
				s.blocks[i] = "text"
				return s.chunk(object{"content": str(c["text"])}, nil, nil), nil
			case "tool_use":
				if err := fields(c, "type id name input"); err != nil {
					return nil, err
				}
				if str(c["id"]) == "" || str(c["name"]) == "" {
					return nil, unsupported("missing tool identity")
				}
				if len(obj(c["input"])) > 0 {
					return nil, unsupported("nonempty streamed tool input at start")
				}
				s.blocks[i] = "tool"
				s.tools[i] = s.next
				s.next++
				return s.chunk(object{"tool_calls": []any{object{"index": s.tools[i], "id": c["id"], "type": "function", "function": object{"name": c["name"], "arguments": ""}}}}, nil, nil), nil
			default:
				return nil, unsupported("stream block " + str(c["type"]))
			}
		case "content_block_delta":
			i := int(number(m["index"]))
			d := obj(m["delta"])
			if d["type"] == "text_delta" && s.blocks[i] == "text" {
				if err := fields(d, "type text"); err != nil {
					return nil, err
				}
				return s.chunk(object{"content": d["text"]}, nil, nil), nil
			}
			if d["type"] == "input_json_delta" && s.blocks[i] == "tool" {
				if err := fields(d, "type partial_json"); err != nil {
					return nil, err
				}
				s.args[i] = s.args[i] || str(d["partial_json"]) != ""
				return s.chunk(object{"tool_calls": []any{object{"index": s.tools[i], "function": object{"arguments": d["partial_json"]}}}}, nil, nil), nil
			}
			return nil, unsupported("stream delta " + str(d["type"]))
		case "content_block_stop":
			i := int(number(m["index"]))
			kind := s.blocks[i]
			delete(s.blocks, i)
			if kind == "" {
				return nil, unsupported("stop for unknown block")
			}
			if kind == "tool" && !s.args[i] {
				return s.chunk(object{"tool_calls": []any{object{"index": s.tools[i], "function": object{"arguments": "{}"}}}}, nil, nil), nil
			}
			return nil, nil
		case "message_delta":
			d := obj(m["delta"])
			if err := fields(d, "stop_reason stop_sequence"); err != nil {
				return nil, err
			}
			reason, err := stop(str(d["stop_reason"]), s.From)
			if err != nil {
				return nil, err
			}
			s.reason = reason
			u := obj(m["usage"])
			if n, ok := u["input_tokens"]; ok {
				s.input = number(n) + s.cachedInput
			}
			if n, ok := u["output_tokens"]; ok {
				s.output = number(n)
			}
			return nil, nil
		case "message_stop":
			if !s.started || s.reason == "" || len(s.blocks) != 0 {
				return nil, fmt.Errorf("premature message_stop")
			}
			s.finished = true
			out := s.chunk(object{}, s.reason, object{"prompt_tokens": s.input, "completion_tokens": s.output, "total_tokens": s.input + s.output})
			return append(out, []byte("data: [DONE]\n\n")...), nil
		default:
			return nil, unsupported("SSE event " + str(m["type"]))
		}
	}
	var out []byte
	if err := fields(m, "id object created model choices usage system_fingerprint service_tier"); err != nil {
		return nil, err
	}
	if str(m["object"]) != "chat.completion.chunk" {
		return nil, unsupported("non-chat SSE")
	}
	if !s.started {
		s.started = true
		s.id = str(m["id"])
		s.model = str(m["model"])
		out = append(out, event("message_start", object{"type": "message_start", "message": object{"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": object{"input_tokens": 0, "output_tokens": 0}}})...)
	}
	if u := obj(m["usage"]); u != nil {
		s.input = number(u["prompt_tokens"])
		s.output = number(u["completion_tokens"])
	}
	choices := arr(m["choices"])
	if len(choices) > 1 {
		return nil, unsupported("multiple stream choices")
	}
	for _, v := range choices {
		c := obj(v)
		if err := fields(c, "index delta finish_reason"); err != nil {
			return nil, err
		}
		if number(c["index"]) != 0 {
			return nil, unsupported("choice index")
		}
		d := obj(c["delta"])
		if err := fields(d, "role content tool_calls"); err != nil {
			return nil, err
		}
		if d["content"] != nil {
			if _, ok := d["content"].(string); !ok {
				return nil, unsupported("non-text streamed content")
			}
		}
		if d["tool_calls"] != nil {
			if _, ok := d["tool_calls"].([]any); !ok {
				return nil, unsupported("invalid streamed tools")
			}
		}
		if role := str(d["role"]); role != "" && role != "assistant" {
			return nil, unsupported("stream role")
		}
		if t := str(d["content"]); t != "" {
			if s.textIndex == nil {
				i := s.next
				s.next++
				s.textIndex = &i
				s.blocks[i] = "text"
				out = append(out, event("content_block_start", object{"type": "content_block_start", "index": i, "content_block": object{"type": "text", "text": ""}})...)
			}
			out = append(out, event("content_block_delta", object{"type": "content_block_delta", "index": *s.textIndex, "delta": object{"type": "text_delta", "text": t}})...)
		}
		for _, v := range arr(d["tool_calls"]) {
			t := obj(v)
			if err := fields(t, "index id type function"); err != nil {
				return nil, err
			}
			f := obj(t["function"])
			if err := fields(f, "name arguments"); err != nil {
				return nil, err
			}
			i := int(number(t["index"]))
			block, ok := s.tools[i]
			if !ok {
				if i < 0 || i >= 1024 {
					return nil, unsupported("invalid stream tool index")
				}
				if str(t["id"]) == "" || str(f["name"]) == "" || str(t["type"]) != "function" {
					return nil, unsupported("fragmented tool identity")
				}
				block = s.next
				s.next++
				s.tools[i] = block
				s.blocks[block] = "tool"
				out = append(out, event("content_block_start", object{"type": "content_block_start", "index": block, "content_block": object{"type": "tool_use", "id": t["id"], "name": f["name"], "input": object{}}})...)
			} else if str(f["name"]) != "" || str(t["id"]) != "" {
				return nil, unsupported("repeated tool identity")
			}
			if args := str(f["arguments"]); args != "" {
				out = append(out, event("content_block_delta", object{"type": "content_block_delta", "index": block, "delta": object{"type": "input_json_delta", "partial_json": args}})...)
			}
		}
		if c["finish_reason"] != nil {
			reason, err := stop(str(c["finish_reason"]), s.From)
			if err != nil {
				return nil, err
			}
			s.reason = reason
		}
	}
	return out, nil
}
