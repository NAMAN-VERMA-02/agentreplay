// Package model is a scripted stand-in for an LLM so the whole pipeline runs
// offline. The model name picks a script ("fake/baseline", "fake/wrong-tool", ...):
// each variant is one planted behaviour change used by the benchmark.
package model

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// Variants lists every script.
var Variants = []string{"baseline", "reworded", "no-search", "wrong-tool", "wrong-path", "wrong-user",
	"swapped-args", "skip-step", "forbidden", "error-as-success", "ignore-result", "bad-format"}

type action struct {
	tool string
	args map[string]any
	text string
}

const defaultPath = "/allowed-root/report.pdf"

// script decides the next action from the variant and the tool results so far.
func script(variant string, results []string) action {
	path := defaultPath
	if len(results) > 0 && strings.HasPrefix(results[0], "/") && variant != "no-search" {
		path = strings.SplitN(results[0], "\n", 2)[0]
	}
	search := action{tool: "search_files", args: map[string]any{"root": "/allowed-root", "query": "quarterly report"}}
	read := action{tool: "read_file", args: map[string]any{"user_id": "u-1", "path": path}}
	stat := action{tool: "stat_backup", args: map[string]any{"path": path}}
	steps := []action{search, read, stat}

	switch variant {
	case "no-search":
		steps = []action{read, stat}
	case "wrong-tool":
		steps[0] = action{tool: "list_dir", args: map[string]any{"path": "/allowed-root"}}
	case "wrong-path":
		steps[0] = action{tool: "search_files", args: map[string]any{"root": "/", "query": "quarterly report"}}
	case "wrong-user":
		steps[1] = action{tool: "read_file", args: map[string]any{"user_id": "u-2", "path": path}}
	case "swapped-args":
		steps[1] = action{tool: "read_file", args: map[string]any{"user_id": path, "path": "u-1"}}
	case "skip-step":
		steps = []action{search, stat}
	case "forbidden":
		steps = append(steps, action{tool: "delete_file", args: map[string]any{"path": path}})
	}
	if len(results) < len(steps) {
		return steps[len(results)]
	}

	backup := "unknown"
	for i, s := range steps {
		if s.tool == "stat_backup" && i < len(results) {
			backup = "ok"
			if strings.HasPrefix(results[i], "ERROR") {
				backup = "missing"
			}
		}
	}
	final := fmt.Sprintf("FILE: %s\nBACKUP: %s", path, backup)
	switch variant {
	case "reworded":
		final = "All done! Here is what I found.\n" + final + "\nLet me know if you need anything else."
	case "error-as-success":
		final = fmt.Sprintf("FILE: %s\nBACKUP: ok", path)
	case "ignore-result":
		final = fmt.Sprintf("FILE: /allowed-root/report.txt\nBACKUP: %s", backup)
	case "bad-format":
		final = "I found the report and its backup is " + backup + "."
	}
	return action{text: final}
}

// Handler serves /v1/messages (Anthropic) and /v1/chat/completions (OpenAI).
// hits counts every request, so tests can prove replay never reached it.
func Handler(hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string            `json:"model"`
			Stream   bool              `json:"stream"`
			Messages []json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil || req.Stream {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		variant := strings.TrimPrefix(req.Model, "fake/")
		known := false
		for _, v := range Variants {
			known = known || v == variant
		}
		if !known {
			http.Error(w, `{"error":"unknown model"}`, http.StatusNotFound)
			return
		}
		openai := strings.HasSuffix(r.URL.Path, "/chat/completions")
		results := toolResults(req.Messages, openai)
		act := script(variant, results)
		in, out := len(body)/4, len(act.text)/4+10
		w.Header().Set("Content-Type", "application/json")
		step := len(results)
		var resp any
		if openai {
			msg := map[string]any{"role": "assistant", "content": nil}
			finish := "tool_calls"
			if act.tool == "" {
				msg["content"], finish = act.text, "stop"
			} else {
				a, _ := json.Marshal(act.args)
				msg["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("call_%d", step), "type": "function",
					"function": map[string]any{"name": act.tool, "arguments": string(a)}}}
			}
			resp = map[string]any{"id": fmt.Sprintf("chatcmpl-fake-%d", step), "object": "chat.completion", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
				"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out}}
		} else {
			content := []any{map[string]any{"type": "text", "text": act.text}}
			stop := "end_turn"
			if act.tool != "" {
				content = []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", step), "name": act.tool, "input": act.args}}
				stop = "tool_use"
			}
			resp = map[string]any{"id": fmt.Sprintf("msg_fake_%d", step), "type": "message", "role": "assistant", "model": req.Model,
				"content": content, "stop_reason": stop, "usage": map[string]any{"input_tokens": in, "output_tokens": out}}
		}
		json.NewEncoder(w).Encode(resp)
	})
}

// toolResults returns the text of every tool result in the conversation, in order.
func toolResults(msgs []json.RawMessage, openai bool) []string {
	var out []string
	for _, raw := range msgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		json.Unmarshal(raw, &m)
		if openai {
			if m.Role == "tool" {
				var s string
				json.Unmarshal(m.Content, &s)
				out = append(out, s)
			}
			continue
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				var s string
				json.Unmarshal(b.Content, &s)
				out = append(out, s)
			}
		}
	}
	return out
}
