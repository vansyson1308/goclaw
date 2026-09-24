// Command deepseekmock is a local stand-in for the DeepSeek API used to test
// GoClaw's DeepSeek integration without network access or spend. It verifies
// the request CONTRACT (it is not the model): the rules below are the ones the
// real API enforces for deepseek-flash / deepseek-v4-pro, and every request
// that breaks one is answered with the same kind of 400 the API returns.
//
//   - Authorization: Bearer <key> must match -key.
//   - model must be a current DeepSeek model name.
//   - Thinking mode is on unless thinking.type == "disabled"; in thinking
//     mode, temperature must not be sent, reasoning_effort must be
//     low|high|max, and once a tool call happened every later assistant
//     message must carry reasoning_content ("must be passed back").
//
// Replies are scripted: -scripts is a directory of JSON files
// {"match": "<text in the conversation>", "steps": [{"tool_calls": [...]} | {"text": "..."}]},
// the same step format as the scripted provider. The step is the number of
// assistant messages already in the request. GET /mock/stats reports what
// was checked, for the harness to assert on.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type step struct {
	Text      string `json:"text,omitempty"`
	ToolCalls []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"tool_calls,omitempty"`
}

type script struct {
	Match string `json:"match"`
	Steps []step `json:"steps"`
	file  string
}

var validModels = map[string]bool{
	"deepseek-flash": true, "deepseek-v4-pro": true,
	// Retired upstream but still routed to V4.1 Flash.
	"deepseek-v4-flash": true, "deepseek-v4-flash-vision-exp": true,
}

type stats struct {
	mu                  sync.Mutex
	Requests            int            `json:"requests"`
	Streamed            int            `json:"streamed"`
	ThinkingRequests    int            `json:"thinking_requests"`
	PassbackChecked     int            `json:"reasoning_passback_checked"` // tool follow-ups that carried reasoning_content
	Violations          map[string]int `json:"violations"`
	Efforts             map[string]int `json:"reasoning_effort"`
	ScriptsUsed         map[string]int `json:"scripts_used"`
	UnmatchedRequests   int            `json:"unmatched_requests"`
	TemperatureAccepted int            `json:"temperature_in_non_thinking"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18993", "listen address")
	key := flag.String("key", "", "expected API key (required)")
	dir := flag.String("scripts", "", "directory of reply scripts")
	flag.Parse()
	if *key == "" || *dir == "" {
		log.Fatal("-key and -scripts are required")
	}
	scripts, err := loadScripts(*dir)
	if err != nil {
		log.Fatal(err)
	}
	st := &stats{Violations: map[string]int{}, Efforts: map[string]int{}, ScriptsUsed: map[string]int{}}
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*key {
				apiError(w, http.StatusUnauthorized, "authentication_error", "Authentication Fails, Your api key is invalid")
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /models", auth(listModels))
	mux.HandleFunc("GET /v1/models", auth(listModels))
	chat := auth(func(w http.ResponseWriter, r *http.Request) { handleChat(w, r, scripts, st) })
	mux.HandleFunc("POST /chat/completions", chat)
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("GET /mock/stats", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	log.Printf("deepseekmock listening on %s with %d scripts", *addr, len(scripts))
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func loadScripts(dir string) ([]script, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []script
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s script
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if s.Match == "" || len(s.Steps) == 0 {
			return nil, fmt.Errorf("%s: match and steps are required", f)
		}
		s.file = filepath.Base(f)
		out = append(out, s)
	}
	return out, nil
}

func listModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{
		{"id": "deepseek-flash", "object": "model", "owned_by": "deepseek"},
		{"id": "deepseek-v4-pro", "object": "model", "owned_by": "deepseek"},
	}})
}

func apiError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": "invalid_request_error"}})
}

func handleChat(w http.ResponseWriter, r *http.Request, scripts []script, st *stats) {
	var req map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.Requests++
	violate := func(rule, msg string) {
		st.Violations[rule]++
		keys := make([]string, 0, len(req))
		for k := range req {
			if k != "messages" && k != "tools" {
				keys = append(keys, fmt.Sprintf("%s=%v", k, req[k]))
			}
		}
		sort.Strings(keys)
		log.Printf("VIOLATION %s: %s (request: %s)", rule, msg, strings.Join(keys, " "))
		apiError(w, http.StatusBadRequest, "invalid_request_error", msg)
	}

	model, _ := req["model"].(string)
	if !validModels[model] {
		violate("model", fmt.Sprintf("Model Not Exist: %q", model))
		return
	}
	thinking := true
	if th, ok := req["thinking"].(map[string]any); ok {
		switch th["type"] {
		case "disabled":
			thinking = false
		case "enabled":
		default:
			violate("thinking_type", fmt.Sprintf("invalid thinking.type %v", th["type"]))
			return
		}
	}
	if eff, ok := req["reasoning_effort"].(string); ok {
		switch eff {
		case "low", "high", "max":
			st.Efforts[eff]++
		case "none":
			thinking = false
			st.Efforts[eff]++
		default:
			violate("reasoning_effort", fmt.Sprintf("invalid reasoning_effort %q", eff))
			return
		}
	}
	if _, has := req["temperature"]; has {
		if thinking {
			violate("temperature_in_thinking", "temperature is not supported in thinking mode")
			return
		}
		st.TemperatureAccepted++
	}
	msgs, _ := req["messages"].([]any)
	_, hasTools := req["tools"]
	if thinking {
		st.ThinkingRequests++
		if hasTools {
			seenToolCall, checked := false, false
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] != "assistant" {
					continue
				}
				if seenToolCall {
					if _, ok := mm["reasoning_content"]; !ok {
						violate("reasoning_passback", "The `reasoning_content` in the thinking mode must be passed back to the API.")
						return
					}
					checked = true
				}
				if tc, _ := mm["tool_calls"].([]any); len(tc) > 0 {
					seenToolCall = true
					if _, ok := mm["reasoning_content"]; !ok {
						violate("reasoning_passback", "The `reasoning_content` in the thinking mode must be passed back to the API.")
						return
					}
					checked = true
				}
			}
			if checked {
				st.PassbackChecked++
			}
		}
	}

	// Pick the script by text in the conversation; the step is the number of
	// assistant turns already taken.
	var conv strings.Builder
	assistants := 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistants++
		}
		if c, ok := mm["content"].(string); ok {
			conv.WriteString(c)
		}
	}
	var sc *script
	for i := range scripts {
		if strings.Contains(conv.String(), scripts[i].Match) {
			sc = &scripts[i]
			break
		}
	}
	var s step
	switch {
	case sc == nil:
		st.UnmatchedRequests++
		s = step{Text: "OK"}
	case assistants < len(sc.Steps):
		st.ScriptsUsed[sc.file]++
		s = sc.Steps[assistants]
	default:
		st.ScriptsUsed[sc.file]++
		s = step{Text: "Done."}
	}
	reasoning := ""
	if thinking {
		reasoning = fmt.Sprintf("(mock reasoning, step %d)", assistants)
	}
	promptTokens := 200 + conv.Len()/4
	usage := map[string]any{
		"prompt_tokens": promptTokens, "completion_tokens": 40, "total_tokens": promptTokens + 40,
		"prompt_cache_hit_tokens": promptTokens / 2, "prompt_cache_miss_tokens": promptTokens - promptTokens/2,
		"completion_tokens_details": map[string]any{"reasoning_tokens": map[bool]int{true: 20, false: 0}[thinking]},
	}
	var toolCalls []map[string]any
	for i, tc := range s.ToolCalls {
		args, _ := json.Marshal(tc.Arguments)
		toolCalls = append(toolCalls, map[string]any{"index": i, "id": fmt.Sprintf("call_%d_%d", assistants, i), "type": "function",
			"function": map[string]any{"name": tc.Name, "arguments": string(args)}})
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	if stream, _ := req["stream"].(bool); stream {
		st.Streamed++
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		chunk := func(delta map[string]any, fin any) map[string]any {
			return map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": model,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": fin}}}
		}
		if reasoning != "" {
			send(chunk(map[string]any{"role": "assistant", "reasoning_content": reasoning}, nil))
		}
		if len(toolCalls) > 0 {
			send(chunk(map[string]any{"tool_calls": toolCalls}, nil))
		} else {
			send(chunk(map[string]any{"content": s.Text}, nil))
		}
		send(chunk(map[string]any{}, finish))
		send(map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	msg := map[string]any{"role": "assistant", "content": s.Text}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "mock", "object": "chat.completion", "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
}
