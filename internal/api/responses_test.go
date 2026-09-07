package api

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"litegate/internal/store"
)

func TestResponsesToChatConversion(t *testing.T) {
	in := `{
		"model":"m",
		"instructions":"be brief",
		"input":[
			{"type":"message","role":"user","content":"weather in 北京?"},
			{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
			{"type":"function_call_output","call_id":"call_9","output":"{\"temp\":25}"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"old"}]}
		],
		"tools":[
			{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}},
			{"type":"web_search"}
		],
		"tool_choice":{"type":"function","name":"get_weather"},
		"max_output_tokens":77,
		"temperature":0.5,
		"reasoning":{"effort":"low"},
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":false}}
	}`
	var req responsesRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatal(err)
	}
	chat, err := responsesToChat(&req)
	if err != nil {
		t.Fatal(err)
	}
	// instructions → system；items → user/assistant(tool_calls)/tool；reasoning 项跳过
	if len(chat.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(chat.Messages))
	}
	if chat.Messages[0].Role != "system" || *chat.Messages[0].Content != "be brief" {
		t.Fatalf("system message = %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || *chat.Messages[1].Content != "weather in 北京?" {
		t.Fatalf("user message = %+v", chat.Messages[1])
	}
	asst := chat.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_9" ||
		asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("assistant toolcall message = %+v", asst)
	}
	tool := chat.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_9" || *tool.Content != "{\"temp\":25}" {
		t.Fatalf("tool message = %+v", tool)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v（web_search 应被跳过）", chat.Tools)
	}
	tc, _ := chat.ToolChoice.(map[string]any)
	if tc == nil || tc["type"] != "function" {
		t.Fatalf("tool_choice = %+v", chat.ToolChoice)
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 77 || chat.Temperature == nil || *chat.Temperature != 0.5 {
		t.Fatalf("sampling params = %+v", chat)
	}
	if chat.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q", chat.ReasoningEffort)
	}
	if chat.ResponseFormat == nil || chat.ResponseFormat.Type != "json_schema" ||
		chat.ResponseFormat.JSONSchema.Strict {
		t.Fatalf("response_format = %+v", chat.ResponseFormat)
	}
	if chat.Stream {
		t.Fatal("stream should default to false")
	}
}

func TestResponsesEndToEndNonStream(t *testing.T) {
	var gotChat map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotChat)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,
			"message":{"role":"assistant","content":"let me check","reasoning_content":"thinking...",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
			"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,
			"prompt_tokens_details":{"cached_tokens":400}}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)
	if err := st.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 2, OutputPrice: 4}); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"m","stream":true,"input":"hi"}`
	rec := do(srv, "POST", "/v1/responses", body,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 上游应收到 chat/completions 请求（带注入的 include_usage）
	if gotChat["stream"] != true {
		t.Fatalf("upstream stream = %v", gotChat["stream"])
	}
	if _, ok := gotChat["stream_options"]; !ok {
		t.Fatalf("stream_options not injected: %v", gotChat)
	}
	msgs, _ := gotChat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("string input should become one user message: %v", msgs)
	}

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Status  string `json:"status"`
		Model   string `json:"model"`
		Output  []map[string]any `json:"output"`
		Usage   struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
	if !strings.HasPrefix(out.ID, "resp_") || out.Object != "response" || out.Status != "completed" {
		t.Fatalf("response envelope = %+v", out)
	}
	if len(out.Output) != 3 {
		t.Fatalf("output items = %d, want 3（reasoning/message/function_call）: %+v", len(out.Output), out.Output)
	}
	if out.Output[0]["type"] != "reasoning" || out.Output[1]["type"] != "message" ||
		out.Output[2]["type"] != "function_call" {
		t.Fatalf("output order = %+v", out.Output)
	}
	if out.Usage.InputTokens != 1000 || out.Usage.OutputTokens != 500 || out.Usage.InputTokensDetails.CachedTokens != 400 {
		t.Fatalf("usage = %+v", out.Usage)
	}

	// 日志：protocol=responses，cache_tokens 落列，成本按缓存 1/10 折算
	// = 600×2/1e6 + 400×0.2/1e6 + 500×4/1e6 = 0.00328
	page, err := st.ListLogs(store.LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found *store.RequestLog
	for i := range page.Items {
		if page.Items[i].Protocol == "responses" {
			found = &page.Items[i]
		}
	}
	if found == nil {
		t.Fatal("no responses log row")
	}
	if found.PromptTokens != 1000 || found.CompletionTokens != 500 || found.CacheTokens != 400 {
		t.Fatalf("log tokens = %+v", found)
	}
	if math.Abs(found.CostUSD-0.00328) > 1e-9 {
		t.Fatalf("log cost = %v, want 0.00328", found.CostUSD)
	}
}

func TestResponsesStreamBridge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["stream_options"]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"th"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		}
		for _, c := range chunks {
			io.WriteString(w, "data: "+c+"\n\n")
			f.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stream := rec.Body.String()
	// 事件顺序必须单调递增
	order := []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.output_item.done",
		"event: response.completed",
	}
	pos := -1
	for _, ev := range order {
		idx := strings.Index(stream, ev)
		if idx < 0 {
			t.Fatalf("missing event %q in stream:\n%s", ev, stream)
		}
		if idx < pos {
			t.Fatalf("event %q out of order", ev)
		}
		pos = idx
	}
	if !strings.Contains(stream, `"delta":"Hel"`) || !strings.Contains(stream, `"delta":"lo"`) {
		t.Fatalf("text deltas missing:\n%s", stream)
	}
	// completed 事件应带完整 output 与 usage
	completed := stream[strings.Index(stream, "event: response.completed"):]
	for _, want := range []string{`"text":"Hello"`, `"arguments":"{\"a\":1}"`,
		`"input_tokens":10`, `"output_tokens":5`, `"cached_tokens":0`} {
		if !strings.Contains(completed, want) {
			t.Fatalf("completed missing %s:\n%s", want, completed)
		}
	}
	page, err := st.ListLogs(store.LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.Items[0].PromptTokens != 10 || page.Items[0].CompletionTokens != 5 {
		t.Fatalf("stream usage not logged: %+v", page.Items)
	}
}

func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	srv, st := newTestServer(t)
	key := mustCreateKey(t, st)
	rec := do(srv, "POST", "/v1/responses",
		`{"model":"m","input":"hi","previous_response_id":"resp_old"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// 无状态：取回/删除已存对象明确报 501 而不是落到管理页
	rec = do(srv, "GET", "/v1/responses/resp_x", "", map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET stored status = %d, want 501", rec.Code)
	}
}

func TestResponsesNoChannelForModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", []string{"other"}, 0)
	key := mustCreateKey(t, st)
	rec := do(srv, "POST", "/v1/responses", `{"model":"missing","input":"hi"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCountTokensLocal(t *testing.T) {
	srv, st := newTestServer(t)
	key := mustCreateKey(t, st)

	// "abcd" 4 个 ASCII ≈ 1 token + 消息结构 4 = 5
	rec := do(srv, "POST", "/v1/messages/count_tokens",
		`{"model":"claude-x","messages":[{"role":"user","content":"abcd"}]}`,
		map[string]string{"X-Api-Key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.InputTokens != 5 {
		t.Fatalf("input_tokens = %d, want 5", out.InputTokens)
	}

	// 中文按 1 token/字符："你好" = 2 + 4 = 6；system "hi" = 1 + 4 = 5；共 11
	rec = do(srv, "POST", "/v1/messages/count_tokens",
		`{"model":"claude-x","system":"hi","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"X-Api-Key": key})
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.InputTokens != 11 {
		t.Fatalf("input_tokens with system = %d, want 11", out.InputTokens)
	}
}

func TestCountTokensForwardsToAnthropicChannel(t *testing.T) {
	var gotPath, gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "anthropic", upstream.URL+"/v1", []string{"claude-x"}, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/messages/count_tokens",
		`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Api-Key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages/count_tokens" || gotKey != "up-key" {
		t.Fatalf("upstream path = %s, key = %s", gotPath, gotKey)
	}
	if !strings.Contains(rec.Body.String(), "42") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
