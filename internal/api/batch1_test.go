package api

// 批次一功能测试：跨模型故障转移、缓存感知路由（前缀亲和）、告警推送配置、
// 上游余额探测。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"litegate/internal/store"
)

func TestAutoPriorityCrossModelFailover(t *testing.T) {
	// m-a 的渠道对请求返回 500（单渠道确定性失败会透传，多候选链应换下一候选）
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()
	good, got := newAutoUpstream(t)

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", bad.URL+"/v1", []string{"m-a"}, 0)
	mustCreateChannel(t, st, "openai", good.URL+"/v1", []string{"m-b"}, 0)
	key := mustCreateAutoKey(t, st, "priority", nil, nil, []string{"m-a", "m-b"})

	rec := postChat(srv, key, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 1 || (*got)[0] != "m-b" {
		t.Fatalf("upstream models = %v, want fallback to m-b", *got)
	}
}

func TestAffinityStickyKey(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	srv, st := newTestServer(t)
	// 同渠道两把密钥：默认随机轮换；前缀亲和应把同会话粘在同一把上
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m"}, 0)
	chans, _ := st.ListChannels("")
	if len(chans) == 0 {
		t.Fatal("no channel")
	}
	ch := &chans[0]
	if len(ch.APIKeys) < 2 {
		// 渠道只有一把 key 时补一把
		ch.APIKeys = append(ch.APIKeys, store.ChannelKey{Key: "up-key-2"})
		if err := st.UpdateChannel(ch); err != nil {
			t.Fatal(err)
		}
	}
	key := mustCreateKey(t, st)

	// 4 次同前缀（首条消息相同、末条不同）的多轮请求
	for i := 0; i < 4; i++ {
		body := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"turn ` +
			string(rune('a'+i)) + `"}]}`
		rec := postChat(srv, key, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status = %d", i, rec.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 4 {
		t.Fatalf("auths = %d requests", len(auths))
	}
	for i := 1; i < 4; i++ {
		if auths[i] != auths[0] {
			t.Fatalf("affinity broken: %v", auths)
		}
	}
	// 不同前缀的会话不应共享亲和键（各自独立选择，这里只验证键生成分裂）
	if conversationAffinityKey("m", []byte(`{"messages":[{"role":"user","content":"x"},{"role":"user","content":"y"}]}`)) ==
		conversationAffinityKey("m", []byte(`{"messages":[{"role":"user","content":"z"},{"role":"user","content":"y"}]}`)) {
		t.Fatal("different prefixes should have different affinity keys")
	}
	if conversationAffinityKey("m", []byte(`{"messages":[{"role":"user","content":"only"}]}`)) != "" {
		t.Fatal("single-message request should have no affinity key")
	}
}

func TestAlertsConfigAndWebhook(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		payloads = append(payloads, v)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer hook.Close()

	srv, _ := newTestServer(t)
	login := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(login.Body.Bytes(), &tok)
	h := map[string]string{"Authorization": "Bearer " + tok.Token}

	// 校验：enabled 但没 webhook 地址 → 400
	rec := do(srv, "PUT", "/api/admin/alerts", `{"enabled":true}`, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing url: status = %d", rec.Code)
	}

	// 正确保存 + 测试推送
	cfg := `{"enabled":true,"webhook_url":"` + hook.URL + `","format":"json","budget_warn_pct":80,"cooldown_min":1}`
	rec = do(srv, "PUT", "/api/admin/alerts", cfg, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "GET", "/api/admin/alerts", "", h)
	var got struct {
		Enabled     bool `json:"enabled"`
		WarnPct     int  `json:"budget_warn_pct"`
		CooldownMin int  `json:"cooldown_min"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Enabled || got.WarnPct != 80 || got.CooldownMin != 1 {
		t.Fatalf("config roundtrip: %s", rec.Body.String())
	}
	rec = do(srv, "POST", "/api/admin/alerts/test", "", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("test alert: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(payloads)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) == 0 || payloads[0]["event"] != "test" {
		t.Fatalf("webhook payload missing: %v", payloads)
	}
}

func TestBalanceProbe(t *testing.T) {
	// DeepSeek 风格余额端点（base_url 带 /v1，查询路径在根上）
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer up-key" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"55.50"}]}`))
	}))
	defer up.Close()

	srv, st := newTestServer(t)
	id, err := st.CreateChannel(&store.Channel{
		Name: "bal-ch", Type: "openai", BaseURL: up.URL + "/v1",
		APIKeys:           []store.ChannelKey{{Key: "up-key"}},
		Models:            []string{"m"},
		Enabled:           true,
		BalanceAPI:        "deepseek",
		BalanceAlertBelow: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	login := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(login.Body.Bytes(), &tok)
	h := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec := do(srv, "POST", "/api/admin/channels/"+strconv.FormatInt(id, 10)+"/balance", "", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("balance: %d %s", rec.Code, rec.Body.String())
	}
	var rep struct {
		Keys []struct {
			Masked    string  `json:"masked"`
			Remaining float64 `json:"remaining"`
			Currency  string  `json:"currency"`
			Error     string  `json:"error"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if len(rep.Keys) != 1 || rep.Keys[0].Remaining != 55.5 || rep.Keys[0].Currency != "CNY" || rep.Keys[0].Error != "" {
		t.Fatalf("balance report: %s", rec.Body.String())
	}

	// openrouter 风格：remaining = credits - usage
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":{"total_credits":10,"total_usage":4.25}}`))
	}))
	defer up2.Close()
	c2 := &store.Channel{
		Name: "or-ch", Type: "openai", BaseURL: up2.URL + "/api/v1",
		APIKeys: []store.ChannelKey{{Key: "k"}}, Enabled: true, BalanceAPI: "openrouter",
	}
	rem, cur, err := fetchBalance(context.Background(), upstreamClientForTest, c2, "k")
	if err != nil || rem != 5.75 || cur != "USD" {
		t.Fatalf("openrouter balance: rem=%v cur=%v err=%v", rem, cur, err)
	}
}
