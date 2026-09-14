package api

// auto 路由测试：model="auto" 的触发条件、四种策略的解析结果、白名单约束与
// /v1/models 的 auto 伪模型。上游用 httptest 记录实际收到的 model 字段。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"litegate/internal/store"
)

// newAutoUpstream 起一个记录每次请求 model 的假上游。
func newAutoUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	got := &[]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&v)
		*got = append(*got, v.Model)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	return up, got
}

func mustCreateAutoKey(t *testing.T, st *store.Store, mode string, allowed, pool, prio []string) string {
	t.Helper()
	k := &store.APIKey{Name: "auto-key", AllowedModels: allowed,
		AutoMode: mode, AutoModels: pool, AutoPriority: prio}
	if err := st.CreateAPIKey(k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k.Key
}

func postChat(srv http.Handler, key, body string) *httptest.ResponseRecorder {
	return do(srv, "POST", "/v1/chat/completions", body,
		map[string]string{"Authorization": "Bearer " + key})
}

func TestAutoRequiresConfig(t *testing.T) {
	srv, st := newTestServer(t)
	up, _ := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m1"}, 0)
	key := mustCreateKey(t, st)

	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auto routing") {
		t.Fatalf("error message unclear: %s", rec.Body.String())
	}
}

func TestAutoLatencyPicksFastest(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"slow-model", "fast-model"}, 0)
	key := mustCreateAutoKey(t, st, "latency", nil, nil, nil)

	// 近 24h 实测：slow 5s，fast 100ms
	for _, l := range []store.RequestLog{
		{Model: "slow-model", Status: 200, LatencyMs: 5000},
		{Model: "fast-model", Status: 200, LatencyMs: 100},
	} {
		if err := st.InsertRequestLog(&l); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 1 || (*got)[0] != "fast-model" {
		t.Fatalf("upstream models = %v, want [fast-model]", *got)
	}
}

func TestAutoBalanceRotates(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m1", "m2"}, 0)
	key := mustCreateAutoKey(t, st, "balance", nil, nil, nil)

	for i := 0; i < 4; i++ {
		rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status = %d", i, rec.Code)
		}
	}
	// 4 次请求应在两个候选间均匀轮转
	counts := map[string]int{}
	for _, m := range *got {
		counts[m]++
	}
	if counts["m1"] != 2 || counts["m2"] != 2 {
		t.Fatalf("rotation uneven: %v (raw %v)", counts, *got)
	}
}

func TestAutoPriorityFallsBack(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	idB := mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m-b"}, 0)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m-a"}, 0)
	key := mustCreateAutoKey(t, st, "priority", nil, nil, []string{"m-b", "m-a"})

	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 1 || (*got)[0] != "m-b" {
		t.Fatalf("upstream models = %v, want [m-b]", *got)
	}

	// 首选渠道停用后自动降级到次选
	ch, err := st.GetChannel(idB)
	if err != nil {
		t.Fatal(err)
	}
	ch.Enabled = false
	if err := st.UpdateChannel(ch); err != nil {
		t.Fatal(err)
	}
	rec = postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 2 || (*got)[1] != "m-a" {
		t.Fatalf("upstream models = %v, want fallback to m-a", *got)
	}
}

func TestAutoSmartByTask(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1",
		[]string{"deepseek-v4-pro", "deepseek-flash", "some-vision-max"}, 0)
	key := mustCreateAutoKey(t, st, "smart", nil, nil, nil)

	// 短文本 → 快档（flash）
	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || (*got)[0] != "deepseek-flash" {
		t.Fatalf("short: status = %d, models = %v, want deepseek-flash", rec.Code, *got)
	}
	// 长文本 → 强档（pro）
	long := strings.Repeat("字", 4000) // 12000 字节 > 6000
	rec = postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"`+long+`"}]}`)
	if rec.Code != http.StatusOK || (*got)[1] != "deepseek-v4-pro" {
		t.Fatalf("long: status = %d, models = %v, want deepseek-v4-pro", rec.Code, *got)
	}
	// 带图片 + 短文本 → 视觉模型里的快档（deepseek-flash 具备图像理解）
	rec = postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":[
		{"type":"text","text":"这是什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`)
	if rec.Code != http.StatusOK || (*got)[2] != "deepseek-flash" {
		t.Fatalf("vision short: status = %d, models = %v, want deepseek-flash", rec.Code, *got)
	}
	// 带图片 + 长文本 → 视觉模型里的强档（some-vision-max）
	rec = postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":[
		{"type":"text","text":"`+long+`"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`)
	if rec.Code != http.StatusOK || (*got)[3] != "some-vision-max" {
		t.Fatalf("vision long: status = %d, models = %v, want some-vision-max", rec.Code, *got)
	}
}

func TestAutoRespectsWhitelistAndPool(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m1", "m2", "m3"}, 0)

	// 白名单限制候选池：即使 latency 统计偏向 m1，也只能选白名单内的模型
	key := mustCreateAutoKey(t, st, "latency", []string{"m2"}, nil, nil)
	if err := st.InsertRequestLog(&store.RequestLog{Model: "m1", Status: 200, LatencyMs: 10}); err != nil {
		t.Fatal(err)
	}
	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if (*got)[0] != "m2" {
		t.Fatalf("whitelist: models = %v, want [m2]", *got)
	}

	// auto_models 显式池：allowed 不限制时只从池里选
	key2 := mustCreateAutoKey(t, st, "latency", nil, []string{"m3"}, nil)
	rec = postChat(srv, key2, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || (*got)[1] != "m3" {
		t.Fatalf("pool: status = %d, models = %v, want [m3]", rec.Code, *got)
	}

	// 池为空且无白名单且没有可服务模型：404
	emptyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer emptyUp.Close()
	srv2, st2 := newTestServer(t)
	_ = srv2
	key3 := mustCreateAutoKey(t, st2, "latency", nil, nil, nil)
	rec = postChat(srv2, key3, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty pool status = %d, want 404", rec.Code)
	}
}

func TestAutoModelsEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	up, _ := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m1"}, 0)
	plain := mustCreateKey(t, st)
	auto := mustCreateAutoKey(t, st, "balance", nil, nil, nil)

	rec := do(srv, "GET", "/v1/models", "", map[string]string{"Authorization": "Bearer " + plain})
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, m := range out.Data {
		if m.ID == "auto" {
			t.Fatalf("auto should not be listed for keys without auto routing")
		}
	}
	rec = do(srv, "GET", "/v1/models", "", map[string]string{"Authorization": "Bearer " + auto})
	out.Data = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "auto" {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto missing for auto key: %s", rec.Body.String())
	}
}

func TestAutoResponsesEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	up, got := newAutoUpstream(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m-a", "m-b"}, 0)
	key := mustCreateAutoKey(t, st, "priority", nil, nil, []string{"m-a", "m-b"})

	rec := do(srv, "POST", "/v1/responses", `{"model":"auto","input":"hi"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 1 || (*got)[0] != "m-a" {
		t.Fatalf("upstream models = %v, want [m-a]", *got)
	}
}

func TestAutoHelpers(t *testing.T) {
	// orderedByPriority：配置序在前，未配置的按原序追加，配置里多余的忽略
	got := orderedByPriority([]string{"c", "x", "a"}, []string{"a", "b", "c"})
	want := []string{"c", "a", "b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("orderedByPriority = %v, want %v", got, want)
	}
	// modelTier 分档
	for m, want := range map[string]int{
		"deepseek-v4-pro": 3, "gpt-max": 3, "nemotron-3-ultra": 3,
		"deepseek-flash": 1, "mini-model": 1, "m3-free": 1,
		"gpt-4": 2, "nemotron-3-super-120b-a12b": 3,
	} {
		if modelTier(m) != want {
			t.Fatalf("modelTier(%s) = %d, want %d", m, modelTier(m), want)
		}
	}
	// analyzeTask：字符串与分块两种 content 形态
	sig := analyzeTask([]byte(`{"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function"}]}`))
	if !sig.tools || sig.chars != 5 {
		t.Fatalf("analyzeTask string = %+v", sig)
	}
	sig = analyzeTask([]byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"abcd"},{"type":"input_image","image_url":{"url":"x"}}]}]}`))
	if !sig.images || sig.chars != 4 {
		t.Fatalf("analyzeTask parts = %+v", sig)
	}
}
