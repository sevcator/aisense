package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type object = map[string]any

// Preserve tool-argument integers exactly, including values above 2^53.
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid trailing JSON")
	}
	return nil
}

func obj(v any) object           { m, _ := v.(map[string]any); return m }
func arr(v any) []any            { a, _ := v.([]any); return a }
func str(v any) string           { s, _ := v.(string); return s }
func unsupported(s string) error { return fmt.Errorf("unsupported cross-format feature: %s", s) }
func fields(m object, allowed string) error {
	for k, v := range m {
		if v != nil && !strings.Contains(" "+allowed+" ", " "+k+" ") {
			return unsupported(k)
		}
	}
	return nil
}
func text(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	a, ok := v.([]any)
	if !ok {
		return "", unsupported("content")
	}
	var b strings.Builder
	for _, v := range a {
		m := obj(v)
		if str(m["type"]) != "text" {
			return "", unsupported("content block " + str(m["type"]))
		}
		if err := fields(m, "type text"); err != nil {
			return "", err
		}
		s, ok := m["text"].(string)
		if !ok {
			return "", unsupported("non-string text")
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// Request converts only the explicitly supported text/tool subset. Native
// requests do not pass through this validator and retain provider extensions.
func Request(b []byte, from string) ([]byte, error) {
	var m object
	if err := decode(b, &m); err != nil {
		return nil, err
	}
	if _, ok := m["messages"].([]any); !ok {
		return nil, unsupported("messages must be an array")
	}
	if m["tools"] != nil {
		if _, ok := m["tools"].([]any); !ok {
			return nil, unsupported("tools must be an array")
		}
	}
	allowed := "model messages stream temperature top_p max_tokens tools tool_choice"
	if from == "openai" {
		allowed += " stop max_completion_tokens n stream_options parallel_tool_calls"
	} else {
		allowed += " system stop_sequences"
	}
	if err := fields(m, allowed); err != nil {
		return nil, err
	}
	out := object{}
	for _, k := range []string{"model", "stream", "temperature", "top_p", "max_tokens"} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	messages := []any{}
	if from == "openai" {
		if n, ok := m["n"]; ok && number(n) != 1 {
			return nil, unsupported("multiple choices")
		}
		if so := obj(m["stream_options"]); so != nil {
			if err := fields(so, "include_usage"); err != nil {
				return nil, err
			}
		}
		if v, ok := m["max_completion_tokens"]; ok {
			out["max_tokens"] = v
		}
		if out["max_tokens"] == nil {
			out["max_tokens"] = 4096
		}
		if v, ok := m["stop"]; ok {
			if s, yes := v.(string); yes {
				out["stop_sequences"] = []string{s}
			} else {
				out["stop_sequences"] = v
			}
		}
		var system []any
		for _, v := range arr(m["messages"]) {
			x := obj(v)
			if err := fields(x, "role content tool_calls tool_call_id"); err != nil {
				return nil, err
			}
			role := str(x["role"])
			t, err := text(x["content"])
			if err != nil {
				return nil, err
			}
			switch role {
			case "system":
				if len(messages) > 0 {
					return nil, unsupported("system message after conversation start")
				}
				system = append(system, object{"type": "text", "text": t})
				continue
			case "tool":
				if str(x["tool_call_id"]) == "" {
					return nil, unsupported("missing tool result ID")
				}
				messages = append(messages, object{"role": "user", "content": []any{object{"type": "tool_result", "tool_use_id": x["tool_call_id"], "content": t}}})
				continue
			case "user", "assistant":
			default:
				return nil, unsupported("role " + role)
			}
			blocks := []any{}
			if t != "" {
				blocks = append(blocks, object{"type": "text", "text": t})
			}
			if x["tool_calls"] != nil {
				if _, ok := x["tool_calls"].([]any); !ok {
					return nil, unsupported("tool_calls must be an array")
				}
			}
			for _, call := range arr(x["tool_calls"]) {
				c := obj(call)
				f := obj(c["function"])
				if str(c["type"]) != "function" || role != "assistant" || str(c["id"]) == "" || str(f["name"]) == "" {
					return nil, unsupported("tool call")
				}
				if err := fields(c, "id type function"); err != nil {
					return nil, err
				}
				if err := fields(f, "name arguments"); err != nil {
					return nil, err
				}
				var input object
				if decode([]byte(str(f["arguments"])), &input) != nil || input == nil {
					return nil, unsupported("non-object tool arguments")
				}
				blocks = append(blocks, object{"type": "tool_use", "id": c["id"], "name": f["name"], "input": input})
			}
			messages = append(messages, object{"role": role, "content": blocks})
		}
		if len(system) > 0 {
			out["system"] = system
		}
	} else {
		if m["system"] != nil {
			t, err := text(m["system"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, object{"role": "system", "content": t})
		}
		if v, ok := m["stop_sequences"]; ok {
			out["stop"] = v
		}
		for _, v := range arr(m["messages"]) {
			x := obj(v)
			if err := fields(x, "role content"); err != nil {
				return nil, err
			}
			role := str(x["role"])
			if role != "user" && role != "assistant" {
				return nil, unsupported("role " + role)
			}
			if s, ok := x["content"].(string); ok {
				messages = append(messages, object{"role": role, "content": s})
				continue
			}
			if _, ok := x["content"].([]any); !ok {
				return nil, unsupported("Messages content must be text or an array")
			}
			before := len(messages)
			var b strings.Builder
			calls := []any{}
			flush := func() {
				if b.Len() > 0 || len(calls) > 0 {
					a := object{"role": role, "content": b.String()}
					if len(calls) > 0 {
						a["tool_calls"] = calls
					}
					messages = append(messages, a)
					b.Reset()
					calls = []any{}
				}
			}
			for _, v := range arr(x["content"]) {
				c := obj(v)
				switch str(c["type"]) {
				case "text":
					if len(calls) > 0 {
						return nil, unsupported("text after tool use in the same message")
					}
					if err := fields(c, "type text"); err != nil {
						return nil, err
					}
					t, ok := c["text"].(string)
					if !ok {
						return nil, unsupported("non-string text")
					}
					b.WriteString(t)
				case "tool_use":
					if role != "assistant" || str(c["id"]) == "" || str(c["name"]) == "" || obj(c["input"]) == nil {
						return nil, unsupported("tool use")
					}
					if err := fields(c, "type id name input"); err != nil {
						return nil, err
					}
					args, _ := json.Marshal(c["input"])
					calls = append(calls, object{"id": c["id"], "type": "function", "function": object{"name": c["name"], "arguments": string(args)}})
				case "tool_result":
					if role != "user" || str(c["tool_use_id"]) == "" {
						return nil, unsupported("tool result")
					}
					if err := fields(c, "type tool_use_id content is_error"); err != nil {
						return nil, err
					}
					if c["is_error"] == true {
						return nil, unsupported("tool result is_error")
					}
					t, err := text(c["content"])
					if err != nil {
						return nil, err
					}
					flush()
					messages = append(messages, object{"role": "tool", "tool_call_id": c["tool_use_id"], "content": t})
				default:
					return nil, unsupported("content block " + str(c["type"]))
				}
			}
			flush()
			if len(messages) == before {
				messages = append(messages, object{"role": role, "content": ""})
			}
		}
	}
	out["messages"] = messages
	if m["tools"] != nil {
		tools := []any{}
		for _, v := range arr(m["tools"]) {
			x := obj(v)
			if from == "openai" {
				if str(x["type"]) != "function" {
					return nil, unsupported("tool definition")
				}
				if err := fields(x, "type function"); err != nil {
					return nil, err
				}
				f := obj(x["function"])
				if err := fields(f, "name description parameters"); err != nil {
					return nil, err
				}
				t := object{"name": f["name"], "input_schema": f["parameters"]}
				if f["description"] != nil {
					t["description"] = f["description"]
				}
				tools = append(tools, t)
			} else {
				if err := fields(x, "name description input_schema"); err != nil {
					return nil, err
				}
				f := object{"name": x["name"], "parameters": x["input_schema"]}
				if x["description"] != nil {
					f["description"] = x["description"]
				}
				tools = append(tools, object{"type": "function", "function": f})
			}
		}
		out["tools"] = tools
	}
	if from == "openai" {
		choice := m["tool_choice"]
		c := object{"type": "auto"}
		switch choice {
		case nil, "auto":
		case "none":
			c["type"] = "none"
		case "required":
			c["type"] = "any"
		default:
			x := obj(choice)
			if str(x["type"]) != "function" {
				return nil, unsupported("tool_choice")
			}
			if err := fields(x, "type function"); err != nil {
				return nil, err
			}
			if err := fields(obj(x["function"]), "name"); err != nil {
				return nil, err
			}
			c = object{"type": "tool", "name": obj(x["function"])["name"]}
		}
		if v, ok := m["parallel_tool_calls"].(bool); ok {
			c["disable_parallel_tool_use"] = !v
		}
		if choice != nil || m["parallel_tool_calls"] != nil {
			out["tool_choice"] = c
		}
	} else if m["tool_choice"] != nil {
		c := obj(m["tool_choice"])
		if err := fields(c, "type name disable_parallel_tool_use"); err != nil {
			return nil, err
		}
		switch str(c["type"]) {
		case "auto", "none":
			out["tool_choice"] = c["type"]
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = object{"type": "function", "function": object{"name": c["name"]}}
		default:
			return nil, unsupported("tool_choice")
		}
		if v, ok := c["disable_parallel_tool_use"].(bool); ok {
			out["parallel_tool_calls"] = !v
		}
	}
	if from == "anthropic" && m["stream"] == true {
		out["stream_options"] = object{"include_usage": true}
	}
	return json.Marshal(out)
}

func stop(reason string, from string) (string, error) {
	if from == "anthropic" {
		switch reason {
		case "end_turn", "stop_sequence":
			return "stop", nil
		case "max_tokens":
			return "length", nil
		case "tool_use":
			return "tool_calls", nil
		}
	} else {
		switch reason {
		case "stop":
			return "end_turn", nil
		case "length":
			return "max_tokens", nil
		case "tool_calls":
			return "tool_use", nil
		}
	}
	return "", unsupported("stop reason " + reason)
}
func usage(v any, from string) object {
	u := obj(v)
	if from == "anthropic" {
		in := number(u["input_tokens"]) + number(u["cache_creation_input_tokens"]) + number(u["cache_read_input_tokens"])
		return object{"prompt_tokens": in, "completion_tokens": number(u["output_tokens"]), "total_tokens": in + number(u["output_tokens"])}
	}
	return object{"input_tokens": number(u["prompt_tokens"]), "output_tokens": number(u["completion_tokens"])}
}
func number(v any) float64 {
	if n, ok := v.(json.Number); ok {
		f, _ := n.Float64()
		return f
	}
	n, _ := v.(float64)
	return n
}
func Error(body []byte, to string) []byte {
	var m object
	_ = json.Unmarshal(body, &m)
	msg := str(obj(m["error"])["message"])
	if msg == "" {
		msg = "upstream protocol error"
	}
	out := object{"error": object{"type": "api_error", "message": msg}}
	if to == "anthropic" {
		out["type"] = "error"
	}
	b, _ := json.Marshal(out)
	return b
}
func Response(b []byte, from string) ([]byte, error) {
	var m object
	if err := decode(b, &m); err != nil {
		return nil, err
	}
	if m["error"] != nil {
		to := "anthropic"
		if from == "anthropic" {
			to = "openai"
		}
		return Error(b, to), nil
	}
	if from == "anthropic" {
		if err := fields(m, "id type role model content stop_reason stop_sequence usage"); err != nil {
			return nil, err
		}
		if str(m["type"]) != "message" {
			return nil, unsupported("invalid Messages response")
		}
		request, _ := json.Marshal(object{"messages": []any{object{"role": "assistant", "content": m["content"]}}})
		converted, err := Request(request, from)
		if err != nil {
			return nil, err
		}
		var c object
		_ = decode(converted, &c)
		msgs := arr(c["messages"])
		if len(msgs) != 1 {
			return nil, unsupported("response content")
		}
		reason, err := stop(str(m["stop_reason"]), from)
		if err != nil {
			return nil, err
		}
		return json.Marshal(object{"id": m["id"], "object": "chat.completion", "created": 0, "model": m["model"], "choices": []any{object{"index": 0, "message": msgs[0], "finish_reason": reason}}, "usage": usage(m["usage"], from)})
	}
	if err := fields(m, "id object created model choices usage system_fingerprint service_tier"); err != nil {
		return nil, err
	}
	choices := arr(m["choices"])
	if len(choices) != 1 {
		return nil, unsupported("response choices")
	}
	c := obj(choices[0])
	if err := fields(c, "index message finish_reason"); err != nil {
		return nil, err
	}
	msg := obj(c["message"])
	if msg["role"] != "assistant" {
		return nil, unsupported("non-assistant response")
	}
	request, _ := json.Marshal(object{"messages": []any{msg}})
	converted, err := Request(request, from)
	if err != nil {
		return nil, err
	}
	var x object
	_ = decode(converted, &x)
	msgs := arr(x["messages"])
	if len(msgs) != 1 {
		return nil, unsupported("response content")
	}
	reason, err := stop(str(c["finish_reason"]), from)
	if err != nil {
		return nil, err
	}
	return json.Marshal(object{"id": m["id"], "type": "message", "role": "assistant", "model": m["model"], "content": obj(msgs[0])["content"], "stop_reason": reason, "stop_sequence": nil, "usage": usage(m["usage"], from)})
}
