package api

// OpenAI Responses API（/v1/responses）桥接：把入站请求转换成 chat/completions
// 走既有渠道路由与故障转移，再把响应（含 SSE 流）转回 Responses 协议。
// 网关无状态：不存 Responses 对象，store=true 视为 false，previous_response_id 报错。
// 这样只支持 chat 的中转渠道也能服务 Codex 等原生 Responses 客户端。

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"litegate/internal/store"
)

// ---------- 入站请求 ----------

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string | item 数组
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens *int64          `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Text *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		} `json:"format"`
	} `json:"text"`
	PreviousResponseID string `json:"previous_response_id"`
	Store              *bool  `json:"store"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem 是 input 数组里的一项；type 缺省按 message 处理。
type responsesItem struct {
	Type      string          `json:"type"` // message | function_call | function_call_output | reasoning | item_reference
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"` // string | 分片数组
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"` // function_call_output 的结果
}

// contentText 把 content/output（字符串或分片数组）拼成纯文本；
// 无法经 chat 透传的分片（如 input_image）跳过。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, pt := range parts {
			switch pt.Type {
			case "", "text", "input_text", "output_text", "summary_text", "refusal":
				b.WriteString(pt.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ---------- chat 侧结构 ----------

// chatToolCall 同时用于构造出站请求与解析上游响应/流式分片。
type chatToolCall struct {
	Index    *int   `json:"index,omitempty"` // 仅流式分片
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatMessageIn 用于构造出站 chat 请求的 messages；Content 为 nil 时输出 null
// （assistant 只带 tool_calls 的标准形态）。
type chatMessageIn struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict bool            `json:"strict"`
	} `json:"json_schema,omitempty"`
}

type chatRequest struct {
	Model           string              `json:"model"`
	Messages        []chatMessageIn     `json:"messages"`
	MaxTokens       *int64              `json:"max_tokens,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	TopP            *float64            `json:"top_p,omitempty"`
	Tools           []chatToolDef       `json:"tools,omitempty"`
	ToolChoice      any                 `json:"tool_choice,omitempty"`
	ResponseFormat  *chatResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	Stream          bool                `json:"stream"`
}

// 上游 chat 响应（非流式 message 与流式 delta 字段不同，分别定义）。

type chatRespMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chatToolCall  `json:"tool_calls"`
}

type chatStreamDelta struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chatToolCall  `json:"tool_calls"`
}

type chatCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int              `json:"index"`
		Message      chatRespMessage  `json:"message"`
		Delta        *chatStreamDelta `json:"delta"`
		FinishReason *string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *chatUsage) tokenUsage() tokenUsage {
	if u == nil {
		return tokenUsage{}
	}
	t := tokenUsage{prompt: u.PromptTokens, completion: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		t.cacheRead = u.PromptTokensDetails.CachedTokens
	}
	return t
}

// ---------- 请求转换 ----------

func responsesToChat(in *responsesRequest) (*chatRequest, error) {
	out := &chatRequest{
		Model: in.Model, MaxTokens: in.MaxOutputTokens,
		Temperature: in.Temperature, TopP: in.TopP, Stream: in.Stream,
	}
	if in.Instructions != "" {
		s := in.Instructions
		out.Messages = append(out.Messages, chatMessageIn{Role: "system", Content: &s})
	}
	if len(in.Input) > 0 {
		var s string
		if json.Unmarshal(in.Input, &s) == nil {
			out.Messages = append(out.Messages, chatMessageIn{Role: "user", Content: &s})
		} else {
			var items []responsesItem
			if err := json.Unmarshal(in.Input, &items); err != nil {
				return nil, fmt.Errorf("invalid input: %w", err)
			}
			for i := range items {
				convertInputItem(out, &items[i])
			}
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // web_search 等工具无 chat 对应物，跳过
		}
		var def chatToolDef
		def.Type = "function"
		def.Function.Name = t.Name
		def.Function.Description = t.Description
		def.Function.Parameters = t.Parameters
		out.Tools = append(out.Tools, def)
	}
	if len(in.ToolChoice) > 0 && string(in.ToolChoice) != "null" {
		var s string
		if json.Unmarshal(in.ToolChoice, &s) == nil {
			switch s {
			case "auto", "none", "required":
				out.ToolChoice = s
			}
		} else {
			var tc struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(in.ToolChoice, &tc) == nil && tc.Name != "" {
				out.ToolChoice = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": tc.Name},
				}
			}
		}
	}
	if in.Text != nil && in.Text.Format != nil {
		f := in.Text.Format
		switch f.Type {
		case "json_object":
			out.ResponseFormat = &chatResponseFormat{Type: "json_object"}
		case "json_schema":
			if len(f.Schema) > 0 && string(f.Schema) != "null" {
				name := f.Name
				if name == "" {
					name = "response"
				}
				rf := &chatResponseFormat{Type: "json_schema"}
				rf.JSONSchema = &struct {
					Name   string          `json:"name"`
					Schema json.RawMessage `json:"schema"`
					Strict bool            `json:"strict"`
				}{Name: name, Schema: f.Schema, Strict: f.Strict == nil || *f.Strict}
				out.ResponseFormat = rf
			}
		}
	}
	if in.Reasoning != nil && in.Reasoning.Effort != "" {
		out.ReasoningEffort = in.Reasoning.Effort
	}
	return out, nil
}

func convertInputItem(out *chatRequest, it *responsesItem) {
	switch it.Type {
	case "", "message":
		text := contentText(it.Content)
		role := it.Role
		switch role {
		case "developer":
			role = "system"
		case "":
			role = "user"
		}
		out.Messages = append(out.Messages, chatMessageIn{Role: role, Content: &text})
	case "function_call":
		callID := it.CallID
		if callID == "" {
			callID = it.ID
		}
		tc := chatToolCall{ID: callID, Type: "function"}
		tc.Function.Name = it.Name
		tc.Function.Arguments = it.Arguments
		// 连续的 function_call 项合并进同一条 assistant 工具调用消息
		if n := len(out.Messages); n > 0 &&
			out.Messages[n-1].Role == "assistant" && out.Messages[n-1].Content == nil {
			out.Messages[n-1].ToolCalls = append(out.Messages[n-1].ToolCalls, tc)
		} else {
			out.Messages = append(out.Messages, chatMessageIn{
				Role: "assistant", ToolCalls: []chatToolCall{tc},
			})
		}
	case "function_call_output":
		text := contentText(it.Output)
		out.Messages = append(out.Messages, chatMessageIn{
			Role: "tool", Content: &text, ToolCallID: it.CallID,
		})
	default:
		// reasoning / item_reference 等无 chat 对应物，跳过
	}
}

// ---------- 响应对象（非流式） ----------

type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type responsesObject struct {
	ID        string  `json:"id"`
	Object    string  `json:"object"`
	CreatedAt int64   `json:"created_at"`
	Status    string  `json:"status"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Model             string          `json:"model"`
	Output            []any           `json:"output"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Usage             *responsesUsage `json:"usage,omitempty"`
}

func chatToResponses(in *responsesRequest, cc *chatCompletion) *responsesObject {
	out := &responsesObject{
		ID: "resp_" + randHexID(), Object: "response",
		CreatedAt: time.Now().Unix(), Status: "completed",
		Model: in.Model, Output: []any{}, ParallelToolCalls: true,
	}
	if len(cc.Choices) > 0 {
		ch := &cc.Choices[0]
		if ch.Message.ReasoningContent != "" {
			it := reasoningItem(ch.Message.ReasoningContent)
			it["id"] = "rs_" + randHexID()
			out.Output = append(out.Output, it)
		}
		if text := contentText(ch.Message.Content); text != "" {
			out.Output = append(out.Output, map[string]any{
				"id": "msg_" + randHexID(), "type": "message", "status": "completed",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text", "text": text, "annotations": []any{},
				}},
			})
		}
		for _, tc := range ch.Message.ToolCalls {
			out.Output = append(out.Output, map[string]any{
				"id": "fc_" + randHexID(), "type": "function_call", "status": "completed",
				"call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
			})
		}
		if ch.FinishReason != nil && *ch.FinishReason == "length" {
			out.Status = "incomplete"
			out.IncompleteDetails = &struct {
				Reason string `json:"reason"`
			}{Reason: "max_output_tokens"}
		}
	}
	if cc.Usage != nil {
		u := cc.Usage
		total := u.TotalTokens
		if total == 0 {
			total = u.PromptTokens + u.CompletionTokens
		}
		ru := &responsesUsage{
			InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, TotalTokens: total,
		}
		if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
			ru.InputTokensDetails = &struct {
				CachedTokens int64 `json:"cached_tokens"`
			}{CachedTokens: u.PromptTokensDetails.CachedTokens}
		}
		out.Usage = ru
	}
	return out
}

func reasoningItem(text string) map[string]any {
	return map[string]any{
		"type": "reasoning", "status": "completed",
		"summary": []map[string]any{{"type": "summary_text", "text": text}},
	}
}

func randHexID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------- 主流程 ----------

// serveResponses 转换请求后复用 chat 渠道路由：鉴权、选渠道、故障转移与
// chat 路径同一套规则；进入 respond* 后不再转移。
func (p *proxy) serveResponses(w http.ResponseWriter, r *http.Request) {
	ak, err := p.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large or unreadable"})
		return
	}
	var in responsesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if in.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if in.PreviousResponseID != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "stateless gateway: previous_response_id is not supported, send the full conversation in input"})
		return
	}
	chatReq, err := responsesToChat(&in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 流式请求补 stream_options.include_usage；上游 400 时对同一渠道去掉重试一次
	attemptBody := chatBody
	usageInjected := false
	if in.Stream {
		if b, ok := injectStreamUsage(chatBody); ok {
			attemptBody = b
			usageInjected = true
		}
	}

	chans, err := p.st.ListChannels("openai")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	chans = enabledOnly(chans)
	chans = filterByModel(chans, in.Model)
	if len(chans) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no enabled channel serves model: " + in.Model})
		return
	}
	if !ak.AllowsModel(in.Model) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "model not allowed for this api key: " + in.Model})
		return
	}

	start := time.Now()
	attempts := orderCandidates(chans)
	if len(attempts) > 3 {
		attempts = attempts[:3]
	}

	var lastErr error
	i := 0
	for i < len(attempts) {
		c := &attempts[i]
		resp, err := p.attemptUpstream(r, c, "/chat/completions", attemptBody)
		if err != nil {
			lastErr = fmt.Errorf("channel %q: %w", c.Name, err)
			i++
			continue
		}
		if usageInjected && resp.StatusCode == http.StatusBadRequest {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			attemptBody = chatBody
			usageInjected = false
			continue
		}
		if isRetryableStatus(resp.StatusCode) && i < len(attempts)-1 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = fmt.Errorf("channel %q: upstream status %d", c.Name, resp.StatusCode)
			i++
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// 上游错误原样透传（与 chat 路径一致），不转换格式
			defer resp.Body.Close()
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			p.logRequest(ak, c, "responses", in.Model, resp.StatusCode,
				time.Since(start), time.Since(start), tokenUsage{}, "")
			return
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			p.respondResponsesStream(w, ak, c, &in, resp, start)
			return
		}
		p.respondResponsesJSON(w, ak, c, &in, resp, start)
		return
	}

	if lastErr != nil {
		log.Printf("proxy responses %s failed: %v", in.Model, lastErr)
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "all channels failed"})
	p.logRequest(ak, nil, "responses", in.Model, http.StatusBadGateway,
		time.Since(start), time.Since(start), tokenUsage{}, errMsg(lastErr))
}

// serveResponsesStored 网关无状态：对已存 Responses 对象的取回/删除明确报 501。
func (p *proxy) serveResponsesStored(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "stateless gateway: stored responses are not supported; keep full state client-side"})
}

func (p *proxy) respondResponsesJSON(w http.ResponseWriter, ak *store.APIKey, c *store.Channel,
	in *responsesRequest, resp *http.Response, start time.Time) {
	defer resp.Body.Close()
	ttfb := time.Since(start)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream response truncated"})
		p.logRequest(ak, c, "responses", in.Model, http.StatusBadGateway,
			time.Since(start), ttfb, tokenUsage{}, err.Error())
		return
	}
	var cc chatCompletion
	if err := json.Unmarshal(raw, &cc); err != nil || cc.Error != nil {
		// 不是可解析的 chat 响应：原样透传给客户端排查
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		msg := ""
		if err != nil {
			msg = err.Error()
		} else if cc.Error != nil {
			msg = cc.Error.Message
		}
		p.logRequest(ak, c, "responses", in.Model, resp.StatusCode,
			time.Since(start), ttfb, cc.Usage.tokenUsage(), msg)
		return
	}
	out, err := json.Marshal(chatToResponses(in, &cc))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	p.logRequest(ak, c, "responses", in.Model, http.StatusOK,
		time.Since(start), ttfb, cc.Usage.tokenUsage(), "")
}

// ---------- 流式转换 ----------

// streamItem 是一个正在累积的输出项（reasoning/message/function_call 之一）。
type streamItem struct {
	kind   string // reasoning | message | function_call
	id     string
	callID string
	name   string
	text   strings.Builder
	outIdx int
}

// responsesStreamTransformer 把上游 chat SSE 增量转成 Responses 事件流。
type responsesStreamTransformer struct {
	respID      string
	model       string
	createdAt   int64
	items       []*streamItem
	toolByIndex map[int]*streamItem
	finishReason string
}

func kindPrefix(kind string) string {
	switch kind {
	case "reasoning":
		return "rs_"
	case "function_call":
		return "fc_"
	default:
		return "msg_"
	}
}

func sseEvent(w http.ResponseWriter, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *responsesStreamTransformer) skeleton(status string) responsesObject {
	return responsesObject{
		ID: t.respID, Object: "response", CreatedAt: t.createdAt,
		Status: status, Model: t.model, Output: []any{}, ParallelToolCalls: true,
	}
}

func (t *responsesStreamTransformer) emitCreated(w http.ResponseWriter) {
	sseEvent(w, "response.created", map[string]any{"response": t.skeleton("in_progress")})
}

func (t *responsesStreamTransformer) emitItemAdded(w http.ResponseWriter, it *streamItem) {
	var item any
	switch it.kind {
	case "reasoning":
		item = map[string]any{
			"id": it.id, "type": "reasoning", "status": "in_progress",
			"summary": []any{},
		}
	case "function_call":
		item = map[string]any{
			"id": it.id, "type": "function_call", "status": "in_progress",
			"call_id": it.callID, "name": it.name, "arguments": "",
		}
	default:
		item = map[string]any{
			"id": it.id, "type": "message", "status": "in_progress", "role": "assistant",
			"content": []any{},
		}
	}
	sseEvent(w, "response.output_item.added", map[string]any{
		"output_index": it.outIdx, "item": item,
	})
}

// ensureItem 返回同类相邻 item（连续推理/文本增量合并为一项），必要时声明新 item。
func (t *responsesStreamTransformer) ensureItem(w http.ResponseWriter, kind string) *streamItem {
	if n := len(t.items); n > 0 && t.items[n-1].kind == kind {
		return t.items[n-1]
	}
	it := &streamItem{kind: kind, id: kindPrefix(kind) + randHexID(), outIdx: len(t.items)}
	t.items = append(t.items, it)
	t.emitItemAdded(w, it)
	return it
}

// transformChunk 处理一条上游 chat chunk 的 data JSON，追加增量事件。
func (t *responsesStreamTransformer) transformChunk(w http.ResponseWriter, payload []byte, u *tokenUsage) {
	var chunk chatCompletion
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		got := chunk.Usage.tokenUsage()
		if got.prompt > 0 {
			u.prompt = got.prompt
			u.cacheRead = got.cacheRead
			u.cacheWrite = got.cacheWrite
		}
		if got.completion > u.completion {
			u.completion = got.completion
		}
	}
	if len(chunk.Choices) == 0 || chunk.Choices[0].Delta == nil {
		return
	}
	delta := chunk.Choices[0].Delta

	if delta.ReasoningContent != "" { // 推理增量（DeepSeek / SiliconFlow 风格）
		it := t.ensureItem(w, "reasoning")
		it.text.WriteString(delta.ReasoningContent)
		sseEvent(w, "response.reasoning_summary_text.delta", map[string]any{
			"item_id": it.id, "output_index": it.outIdx, "delta": delta.ReasoningContent,
		})
	}
	if s := contentText(delta.Content); s != "" {
		it := t.ensureItem(w, "message")
		it.text.WriteString(s)
		sseEvent(w, "response.output_text.delta", map[string]any{
			"item_id": it.id, "output_index": it.outIdx, "delta": s,
		})
	}
	for _, tc := range delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		it := t.toolByIndex[idx]
		if it == nil {
			it = &streamItem{
				kind: "function_call", id: "fc_" + randHexID(),
				callID: tc.ID, name: tc.Function.Name, outIdx: len(t.items),
			}
			if it.callID == "" {
				it.callID = "call_" + randHexID()
			}
			t.items = append(t.items, it)
			if t.toolByIndex == nil {
				t.toolByIndex = map[int]*streamItem{}
			}
			t.toolByIndex[idx] = it
			t.emitItemAdded(w, it)
		}
		if frag := tc.Function.Arguments; frag != "" {
			it.text.WriteString(frag)
			sseEvent(w, "response.function_call_arguments.delta", map[string]any{
				"item_id": it.id, "output_index": it.outIdx, "delta": frag,
			})
		}
	}
	if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
		t.finishReason = *fr
	}
}

// finish 收尾：逐项 output_item.done，再发 response.completed（含 usage 与完整 output）。
func (t *responsesStreamTransformer) finish(w http.ResponseWriter, u tokenUsage, upstreamErr bool) {
	output := make([]any, 0, len(t.items))
	for _, it := range t.items {
		var done any
		switch it.kind {
		case "reasoning":
			done = reasoningItem(it.text.String())
			done.(map[string]any)["id"] = it.id
		case "function_call":
			done = map[string]any{
				"id": it.id, "type": "function_call", "status": "completed",
				"call_id": it.callID, "name": it.name, "arguments": it.text.String(),
			}
		default:
			done = map[string]any{
				"id": it.id, "type": "message", "status": "completed", "role": "assistant",
				"content": []map[string]any{{
					"type": "output_text", "text": it.text.String(), "annotations": []any{},
				}},
			}
		}
		sseEvent(w, "response.output_item.done", map[string]any{
			"output_index": it.outIdx, "item": done,
		})
		output = append(output, done)
	}
	obj := t.skeleton("completed")
	obj.Output = output
	if t.finishReason == "length" {
		obj.Status = "incomplete"
		obj.IncompleteDetails = &struct {
			Reason string `json:"reason"`
		}{Reason: "max_output_tokens"}
	}
	obj.Usage = &responsesUsage{
		InputTokens: u.prompt, OutputTokens: u.completion,
		TotalTokens: u.prompt + u.completion,
	}
	obj.Usage.InputTokensDetails = &struct {
		CachedTokens int64 `json:"cached_tokens"`
	}{CachedTokens: u.cacheRead}
	event := "response.completed"
	if upstreamErr {
		obj.Status = "failed"
		obj.Error = &struct {
			Message string `json:"message"`
		}{Message: "upstream stream interrupted"}
		event = "response.failed"
	}
	sseEvent(w, event, map[string]any{"response": obj})
}

// respondResponsesStream 读上游 chat SSE 并写 Responses 事件流。
func (p *proxy) respondResponsesStream(w http.ResponseWriter, ak *store.APIKey, c *store.Channel,
	in *responsesRequest, resp *http.Response, start time.Time) {
	defer resp.Body.Close()
	ttfb := time.Since(start)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	tr := &responsesStreamTransformer{
		respID: "resp_" + randHexID(), model: in.Model, createdAt: time.Now().Unix(),
	}
	tr.emitCreated(w)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var u tokenUsage
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		tr.transformChunk(w, payload, &u)
	}
	err := sc.Err()
	tr.finish(w, u, err != nil)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	p.logRequest(ak, c, "responses", in.Model, http.StatusOK, time.Since(start), ttfb, u, msg)
}

// ---------- count_tokens ----------

// serveCountTokens 实现 Anthropic count_tokens：有 anthropic 渠道可服务该模型时
// 转发上游精确值；否则本地启发式估算（零上游调用、不记请求日志）。
func (p *proxy) serveCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, err := p.authenticate(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	var req struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model and messages are required"})
		return
	}
	if chans, err := p.st.ListChannels("anthropic"); err == nil {
		chans = enabledOnly(chans)
		chans = filterByModel(chans, req.Model)
		if len(chans) > 0 {
			if resp, err := p.attemptUpstream(r, &chans[0], "/messages/count_tokens", body); err == nil {
				defer resp.Body.Close()
				if ct := resp.Header.Get("Content-Type"); ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"input_tokens": estimateTokens(req.Messages) + estimateTokens(req.System) +
			estimateToolsTokens(req.Tools),
	})
}

type ctMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// estimateTokens 估算 messages（数组）或 system（字符串/分片数组）的 token 数。
func estimateTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var msgs []ctMessage
	if json.Unmarshal(raw, &msgs) == nil && len(msgs) > 0 {
		isMessages := false
		for _, m := range msgs {
			if m.Role != "" {
				isMessages = true
				break
			}
		}
		if isMessages {
			var total int64
			for _, m := range msgs {
				total += 4 + estText(contentText(m.Content))
			}
			return total
		}
	}
	return 4 + estText(contentText(raw))
}

// estimateToolsTokens 估算工具定义的 token：按原始 JSON 文本近似 + 每个工具 8 token 结构开销。
func estimateToolsTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var tools []map[string]any
	if json.Unmarshal(raw, &tools) != nil {
		return 0
	}
	return int64(len(tools))*8 + estText(string(raw))
}

// estText 启发式估算纯文本 token：ASCII ≈ 4 字符/token，CJK 等非 ASCII ≈ 1 token/字符。
// 无上游精确计数时的近似；方向上宁可略高估，对上下文压缩触发阈值更安全。
func estText(s string) int64 {
	if s == "" {
		return 0
	}
	var f float64
	for _, r := range s {
		if r < 128 {
			f += 0.25
		} else {
			f += 1.0
		}
	}
	return int64(math.Ceil(f))
}
