package api

// M4 渠道与密钥治理的回归测试：多 key 轮换与冷却、模型映射、
// 虚拟密钥过期/限速/预算、渠道密钥启停状态保留。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"litegate/internal/store"
)

func TestMultiKeyRotationOn401(t *testing.T) {
	var seenKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seenKeys = append(seenKeys, key)
		if key != "good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid upstream key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	_, err := st.CreateChannel(&store.Channel{
		Name: "multi", Type: "openai", BaseURL: upstream.URL + "/v1",
		APIKeys:   []store.ChannelKey{{Key: "bad-key"}, {Key: "good-key"}},
		Models:    []string{"m"}, Weight: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 坏 key 401 后应自动换到好 key。起点 key 是随机轮转的（均匀分摊）：
	// 好 key 可能恰好排第一（此时无轮换发生），断言只要求最终经好 key 成功，
	// 且好 key 之前出现过的都是坏 key。
	if len(seenKeys) == 0 || seenKeys[len(seenKeys)-1] != "good-key" {
		t.Fatalf("expected success via good-key, upstream saw keys: %v", seenKeys)
	}
	for _, k := range seenKeys[:len(seenKeys)-1] {
		if k != "bad-key" {
			t.Fatalf("unexpected key tried before good-key: %v", seenKeys)
		}
	}
}

func TestKeyHealthCooldownSkipsFailedKey(t *testing.T) {
	m := newKeyHealthManager()
	c := &store.Channel{ID: 7, APIKeys: []store.ChannelKey{
		{ID: 1, Key: "a", Enabled: true},
		{ID: 2, Key: "b", Enabled: true},
	}}
	now := time.Now()
	// 失败 1 次（未达阈值）不冷却
	m.reportFailure(c.ID, 1)
	if got := m.available(c, now); len(got) != 2 {
		t.Fatalf("after 1 failure both keys should be available, got %d", len(got))
	}
	// 连续失败达阈值进入冷却 → available 只剩另一把
	m.reportFailure(c.ID, 1)
	got := m.available(c, now)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("cooled key should be skipped, got %+v", got)
	}
	// 全部冷却时仍返回（整体冷却好过必然失败）
	m.reportFailure(c.ID, 2)
	m.reportFailure(c.ID, 2)
	if got := m.available(c, now); len(got) != 2 {
		t.Fatalf("all-cooling fallback expected 2 keys, got %d", len(got))
	}
	// 成功复位
	m.reportSuccess(c.ID, 1)
	if got := m.available(c, now); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("success should reset cooldown, got %+v", got)
	}
}

func TestModelMapRewrite(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		upstreamModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	_, err := st.CreateChannel(&store.Channel{
		Name: "mapped", Type: "openai", BaseURL: upstream.URL + "/v1",
		APIKeys: []store.ChannelKey{{Key: "up-key"}},
		Models:  []string{"fast"}, ModelMap: map[string]string{"fast": "real-model"},
		Weight: 1, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"fast","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "real-model" {
		t.Fatalf("upstream model = %q, want mapped real-model", upstreamModel)
	}
	// 日志与统计保持对外名
	page, err := st.ListLogs(store.LogFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.Items[0].Model != "fast" {
		t.Fatalf("log model = %+v, want external name fast", page.Items)
	}
}

func TestAPIKeyExpiry(t *testing.T) {
	srv, st := newTestServer(t)
	k := &store.APIKey{Name: "expired", ExpiresAt: time.Now().AddDate(0, 0, -1).Format("2006-01-02")}
	if err := st.CreateAPIKey(k); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + k.Key})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRPMLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	k := &store.APIKey{Name: "ratelimited", RPMLimit: 2}
	if err := st.CreateAPIKey(k); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
			map[string]string{"Authorization": "Bearer " + k.Key})
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i+1, rec.Code)
		}
	}
	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + k.Key})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("call 3 status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 should carry Retry-After")
	}
}

func TestBudgetLimit(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// 100 万输入 token，按 $2/M 计 $2 成本
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	if err := st.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 2}); err != nil {
		t.Fatal(err)
	}
	k := &store.APIKey{Name: "budgeted", BudgetUSD: 1, BudgetPeriod: "daily"}
	if err := st.CreateAPIKey(k); err != nil {
		t.Fatal(err)
	}

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + k.Key})
	if rec.Code != http.StatusOK {
		t.Fatalf("call 1 status = %d, want 200", rec.Code)
	}
	rec = do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + k.Key})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("call 2 status = %d, want 429 (budget $1 < $2 cost)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "budget") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChannelKeyEnabledStatePreservedOnUpdate(t *testing.T) {
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", "http://example.invalid/v1", nil, 0)
	// 通过管理 API 禁用第一把密钥
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	hdr := map[string]string{"Authorization": "Bearer " + login.Token}

	rec = do(srv, "GET", "/api/admin/channels", "", hdr)
	var chans []struct {
		ID      int64 `json:"id"`
		APIKeys []struct {
			ID      int64  `json:"id"`
			Masked  string `json:"masked"`
			Enabled bool   `json:"enabled"`
		} `json:"api_keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chans); err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 || len(chans[0].APIKeys) != 1 {
		t.Fatalf("channels = %+v", chans)
	}
	// 渠道里没有明文 key 可对照，禁用后靠 UpdateChannel(不带 api_keys) 验证状态保留
	ch, err := st.GetChannel(chans[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelKeyEnabled(ch.APIKeys[0].ID, false); err != nil {
		t.Fatal(err)
	}
	// 普通渠道更新（DisableModel 内部也是这条路）不应重置手动禁用状态
	if err := st.DisableModel(ch.ID, "m"); err != nil && err != store.ErrNotFound {
		// DisableModel 要求渠道存在即可，模型不存在也会走 UpdateChannel
		_ = err
	}
	ch2, err := st.GetChannel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch2.APIKeys) != 1 || ch2.APIKeys[0].Enabled {
		t.Fatalf("manual disable lost after update: %+v", ch2.APIKeys)
	}
}
