package api

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"litegate/internal/store"
)

func adminLogin(t *testing.T, srv http.Handler) string {
	t.Helper()
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("login failed: %s", rec.Body.String())
	}
	return out.Token
}

func TestInjectStreamUsage(t *testing.T) {
	out, ok := injectStreamUsage([]byte(`{"model":"m","stream":true,"messages":[]}`))
	if !ok {
		t.Fatal("stream request should be injected")
	}
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("injected body = %s", out)
	}
	cases := []string{
		`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, // 已自带
		`{"model":"m","messages":[]}`, // 非流式
		`not-json`,
	}
	for _, in := range cases {
		if _, ok := injectStreamUsage([]byte(in)); ok {
			t.Fatalf("should not inject: %s", in)
		}
	}
}

func TestUsageFromJSON(t *testing.T) {
	cases := []struct {
		body       string
		prompt     int64
		completion int64
		ok         bool
	}{
		{`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`, 10, 5, true},       // openai
		{`{"usage":{"input_tokens":7,"output_tokens":3}}`, 7, 3, true},              // anthropic
		{`{"message":{"usage":{"input_tokens":25,"output_tokens":1}}}`, 25, 1, true}, // anthropic message_start
		{`{"usage":null}`, 0, 0, false},
		{`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, 0, 0, false},
		{`not-json`, 0, 0, false},
	}
	for _, c := range cases {
		u, ok := usageFromJSON([]byte(c.body))
		if ok != c.ok || u.prompt != c.prompt || u.completion != c.completion {
			t.Fatalf("usageFromJSON(%s) = %+v, %v; want %d/%d %v", c.body, u, ok, c.prompt, c.completion, c.ok)
		}
	}
}

func TestSSEScannerAcrossChunks(t *testing.T) {
	// anthropic 两个事件被拆在任意位置写入，usage 仍应拼齐
	events := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\ndata: [DONE]\n\n"
	for _, size := range []int{7, 16, 64, 4096} { // 故意从半行中间切开
		var s sseUsageScanner
		for i := 0; i < len(events); i += size {
			end := i + size
			if end > len(events) {
				end = len(events)
			}
			if _, err := s.Write([]byte(events[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		if !s.found || s.usage.prompt != 25 || s.usage.completion != 42 {
			t.Fatalf("size=%d: usage = %+v found=%v", size, s.usage, s.found)
		}
	}
}

func TestMatchPrice(t *testing.T) {
	prices := []store.ModelPrice{
		{Model: "gpt-4o", InputPrice: 2, OutputPrice: 4},
		{Model: "gpt-4o-mini", InputPrice: 0.15, OutputPrice: 0.6},
		{Model: "anthropic/claude-3", InputPrice: 1, OutputPrice: 1},
	}
	if p := matchPrice(prices, "gpt-4o"); p.Model != "gpt-4o" {
		t.Fatalf("exact match failed: %+v", p)
	}
	if p := matchPrice(prices, "gpt-4o-2024-08-06"); p == nil || p.Model != "gpt-4o" {
		t.Fatalf("prefix fallback failed: %+v", p)
	}
	if p := matchPrice(prices, "gpt-4o-mini-2024-07-18"); p == nil || p.Model != "gpt-4o-mini" {
		t.Fatalf("longest prefix failed: %+v", p)
	}
	if p := matchPrice(prices, "anthropic/claude-3-haiku"); p == nil || p.Model != "anthropic/claude-3" {
		t.Fatalf("slash prefix failed: %+v", p)
	}
	if p := matchPrice(prices, "gpt-4omni"); p != nil {
		t.Fatalf("non-segment prefix should not match: %+v", p)
	}
	if matchPrice(prices, "") != nil || matchPrice(prices, "unknown") != nil {
		t.Fatal("missing model should return nil")
	}
}

func TestCostOf(t *testing.T) {
	if store.CostOf(nil, 1000, 1000) != 0 {
		t.Fatal("nil price should cost 0")
	}
	p := &store.ModelPrice{InputPrice: 2, OutputPrice: 4}
	if got := store.CostOf(p, 1_000_000, 500_000); got != 4 {
		t.Fatalf("cost = %v, want 4", got)
	}
	// 3 个 prompt token × 0.5 美元/百万 = 0.0000015，四舍五入到 6 位小数
	p2 := &store.ModelPrice{InputPrice: 0.5}
	if got := store.CostOf(p2, 3, 0); math.Abs(got-0.000002) > 1e-9 {
		t.Fatalf("cost rounding = %v", got)
	}
}

func TestProxyLogsUsageAndCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)
	if err := st.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 2, OutputPrice: 4}); err != nil {
		t.Fatal(err)
	}

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	page, err := st.ListLogs(store.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("logs = %d", len(page.Items))
	}
	l := page.Items[0]
	if l.PromptTokens != 1000 || l.CompletionTokens != 500 {
		t.Fatalf("tokens = %d/%d", l.PromptTokens, l.CompletionTokens)
	}
	// 1000/1e6*2 + 500/1e6*4 = 0.004
	if math.Abs(l.CostUSD-0.004) > 1e-9 {
		t.Fatalf("cost = %v, want 0.004", l.CostUSD)
	}
	if l.TtfbMs < 0 || l.LatencyMs < 0 {
		t.Fatalf("negative latency: ttfb=%d total=%d", l.TtfbMs, l.LatencyMs)
	}
}

func TestStreamUsageCapturedAndInjected(t *testing.T) {
	var gotStreamOptions bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotStreamOptions = strings.Contains(string(b), "include_usage")
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		f.Flush()
		// 末尾 chunk 携带 usage（上游在 stream_options.include_usage 下的行为）
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !gotStreamOptions {
		t.Fatal("stream_options.include_usage not injected to upstream")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream body damaged: %s", rec.Body.String())
	}
	page, err := st.ListLogs(store.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].PromptTokens != 11 || page.Items[0].CompletionTokens != 7 {
		t.Fatalf("stream usage not logged: %+v", page.Items)
	}
}

func TestAnthropicStreamUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n")
		f.Flush()
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "anthropic", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/messages", `{"model":"claude-x","stream":true,"max_tokens":16,"messages":[]}`,
		map[string]string{"X-Api-Key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	page, err := st.ListLogs(store.LogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].PromptTokens != 25 || page.Items[0].CompletionTokens != 42 {
		t.Fatalf("anthropic usage not merged: %+v", page.Items)
	}
}

// 上游不认识 stream_options 时返回 400：网关应去掉该字段对同一渠道重试并成功。
func TestStreamOptionsFallbackOn400(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls++
		if strings.Contains(string(b), "stream_options") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"unknown field stream_options"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (injected then retried)", calls)
	}
	page, _ := st.ListLogs(store.LogFilter{})
	if len(page.Items) != 1 || page.Items[0].Status != 200 || page.Items[0].PromptTokens != 3 {
		t.Fatalf("log = %+v", page.Items)
	}
}

func TestPriceAdminAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	tok := adminLogin(t, srv)
	h := map[string]string{"Authorization": "Bearer " + tok}

	rec := do(srv, "PUT", "/api/admin/prices", `{"model":"gpt-4o","input_price":2,"output_price":4}`, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d, %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "GET", "/api/admin/prices", "", h)
	if !strings.Contains(rec.Body.String(), "gpt-4o") {
		t.Fatalf("list = %s", rec.Body.String())
	}
	rec = do(srv, "PUT", "/api/admin/prices", `{"model":"  ","input_price":1}`, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid put = %d", rec.Code)
	}
	rec = do(srv, "PUT", "/api/admin/prices", `{"model":"m","input_price":-1}`, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative put = %d", rec.Code)
	}
	rec = do(srv, "DELETE", "/api/admin/prices/gpt-4o", "", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = do(srv, "DELETE", "/api/admin/prices/gpt-4o", "", h)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete = %d, want 404", rec.Code)
	}
}

func TestLogsFilterAndPagination(t *testing.T) {
	srv, st := newTestServer(t)
	c1, _ := st.CreateChannel(&store.Channel{Name: "a", Type: "openai", BaseURL: "http://x/v1", Enabled: true})
	c2, _ := st.CreateChannel(&store.Channel{Name: "b", Type: "openai", BaseURL: "http://y/v1", Enabled: true})
	logs := []store.RequestLog{
		{ChannelID: c1, Model: "m1", Status: 200, PromptTokens: 10, CostUSD: 0.1},
		{ChannelID: c1, Model: "m2", Status: 500, Error: "boom"},
		{ChannelID: c2, Model: "m1", Status: 200, PromptTokens: 20, CostUSD: 0.2},
		{ChannelID: c2, Model: "m3", Status: 429, Error: "rate"},
		{ChannelID: c2, Model: "m3", Status: 200},
	}
	for i := range logs {
		if err := st.InsertRequestLog(&logs[i]); err != nil {
			t.Fatal(err)
		}
	}
	tok := adminLogin(t, srv)
	h := map[string]string{"Authorization": "Bearer " + tok}

	get := func(query string) store.LogPage {
		t.Helper()
		rec := do(srv, "GET", "/api/admin/logs"+query, "", h)
		if rec.Code != http.StatusOK {
			t.Fatalf("logs %s = %d", query, rec.Code)
		}
		var p store.LogPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := get("?channel_id=" + strconv.FormatInt(c1, 10)); p.Total != 2 || len(p.Items) != 2 {
		t.Fatalf("channel filter: %+v", p)
	}
	if p := get("?status=error"); p.Total != 2 {
		t.Fatalf("error filter: %+v", p)
	}
	if p := get("?status=ok"); p.Total != 3 {
		t.Fatalf("ok filter: %+v", p)
	}
	if p := get("?model=m3"); p.Total != 2 {
		t.Fatalf("model filter: %+v", p)
	}
	p := get("?limit=2&offset=1")
	if p.Total != 5 || len(p.Items) != 2 {
		t.Fatalf("pagination: total=%d items=%d", p.Total, len(p.Items))
	}
	// id 降序、offset=1 → 跳过最新的 ID5，取 ID4（m3, 429）与 ID3（m1, 200）
	if p.Items[0].ID != logs[3].ID || p.Items[1].ID != logs[2].ID {
		t.Fatalf("order: %+v", p.Items)
	}
	if p := get("?until=" + url.QueryEscape("2000-01-01 00:00:00")); p.Total != 0 {
		t.Fatalf("until filter: %+v", p)
	}
	if p := get("?since=" + url.QueryEscape("2000-01-01 00:00:00")); p.Total != 5 {
		t.Fatalf("since filter: %+v", p)
	}
}

func TestDashboardUsage(t *testing.T) {
	srv, st := newTestServer(t)
	cid, _ := st.CreateChannel(&store.Channel{Name: "main", Type: "openai", BaseURL: "http://x/v1", Enabled: true})
	logs := []store.RequestLog{
		{ChannelID: cid, Model: "m1", Status: 200, PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.5},
		{ChannelID: cid, Model: "m1", Status: 200, PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.25},
		{ChannelID: cid, Model: "m2", Status: 500, Error: "boom"},
	}
	for i := range logs {
		if err := st.InsertRequestLog(&logs[i]); err != nil {
			t.Fatal(err)
		}
	}
	tok := adminLogin(t, srv)
	rec := do(srv, "GET", "/api/admin/dashboard", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", rec.Code)
	}
	var d store.Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.TodayRequests != 3 || d.TodayErrors != 1 {
		t.Fatalf("today = %d/%d", d.TodayRequests, d.TodayErrors)
	}
	if d.TodayPromptTokens != 110 || d.TodayCompletionTokens != 55 {
		t.Fatalf("today tokens = %d/%d", d.TodayPromptTokens, d.TodayCompletionTokens)
	}
	if math.Abs(d.TodayCostUSD-0.75) > 1e-9 {
		t.Fatalf("today cost = %v", d.TodayCostUSD)
	}
	if len(d.Daily) != 1 || d.Daily[0].Requests != 3 {
		t.Fatalf("daily = %+v", d.Daily)
	}
	if len(d.ByModel) != 2 || d.ByModel[0].Model != "m1" || d.ByModel[0].Requests != 2 {
		t.Fatalf("by_model = %+v", d.ByModel)
	}
	if len(d.ByChannel) != 1 || d.ByChannel[0].ChannelName != "main" || d.ByChannel[0].Requests != 3 {
		t.Fatalf("by_channel = %+v", d.ByChannel)
	}
}
