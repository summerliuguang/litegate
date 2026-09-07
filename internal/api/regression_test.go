package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"litegate/internal/store"
)

// P0-1 回归：停用的渠道不接流量、模型不进 /v1/models。
func TestDisabledChannelNotServed(t *testing.T) {
	srv, st := newTestServer(t)
	key := mustCreateKey(t, st)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()
	id := mustCreateChannel(t, st, "openai", upstream.URL, []string{"test-model"}, 1)
	ch, err := st.GetChannel(id)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	ch.Enabled = false
	if err := st.UpdateChannel(ch); err != nil {
		t.Fatalf("disable channel: %v", err)
	}

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"test-model","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("停用渠道应 404，得到 %d", rec.Code)
	}
	rec = do(srv, "GET", "/v1/models", "", map[string]string{"Authorization": "Bearer " + key})
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 /v1/models: %v", err)
	}
	for _, m := range body.Data {
		if m.ID == "test-model" {
			t.Fatal("停用渠道的模型不应出现在 /v1/models")
		}
	}
}

// P0-2 回归：POST /api/admin/channels 必须保留 disabled_models。
func TestCreateChannelKeepsDisabledModels(t *testing.T) {
	srv, st := newTestServer(t)
	tok := adminToken(t, srv)

	body := `{"name":"ch","type":"openai","base_url":"http://127.0.0.1:1/v1","api_key":"sk-x",` +
		`"models":["a","b"],"disabled_models":["c"],"weight":1,"priority":0,"enabled":true}`
	if rec := do(srv, "POST", "/api/admin/channels", body,
		map[string]string{"Authorization": "Bearer " + tok}); rec.Code != 200 {
		t.Fatalf("创建渠道失败: %d %s", rec.Code, rec.Body.String())
	}
	chans, err := st.ListChannels("")
	if err != nil || len(chans) != 1 {
		t.Fatalf("list channels: %v", err)
	}
	c := chans[0]
	if len(c.DisabledModels) != 1 || c.DisabledModels[0] != "c" {
		t.Fatalf("disabled_models 应保留 [c]，得到 %v", c.DisabledModels)
	}
	if c.ServesModel("c") {
		t.Fatal("禁用模型不应被服务")
	}
	if !c.ServesModel("a") || !c.ServesModel("b") {
		t.Fatal("启用模型应被服务")
	}
}

func adminToken(t *testing.T, srv http.Handler) string {
	t.Helper()
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("登录失败: %d", rec.Code)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Token
}

// P1 回归：通配渠道 EnableModel 只解除禁用，不退化为显式单模型渠道。
func TestEnableModelOnWildcardChannel(t *testing.T) {
	_, st := newTestServer(t)
	id := mustCreateChannel(t, st, "openai", "http://127.0.0.1:1/v1", nil, 1)
	if err := st.DisableModel(id, "some-model"); err != nil {
		t.Fatalf("disable model: %v", err)
	}
	if err := st.EnableModel(id, "some-model"); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	c, err := st.GetChannel(id)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if len(c.Models) != 0 {
		t.Fatalf("通配渠道 enable 后 models 应仍为空，得到 %v", c.Models)
	}
	if !c.ServesModel("any-other-model") {
		t.Fatal("通配渠道 enable 后应继续服务任意模型")
	}
}

// P1 回归：injectStreamUsage 不得破坏大整数字段。
func TestInjectStreamUsagePreservesBigNumbers(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"seed":1234567890123456789,"count":1000000}`
	out, ok := injectStreamUsage([]byte(body))
	if !ok {
		t.Fatal("应注入 stream_options")
	}
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("重组后 JSON 非法: %v", err)
	}
	if got := v["seed"].(json.Number).String(); got != "1234567890123456789" {
		t.Fatalf("seed 精度丢失: %s", got)
	}
	if got := v["count"].(json.Number).String(); got != "1000000" {
		t.Fatalf("count 被改写: %s", got)
	}
	so, _ := v["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatal("stream_options.include_usage 未注入")
	}
}

// 安全：同一来源连续 5 次密码错误后应被限速（429），正确密码也拒绝。
func TestLoginRateLimited(t *testing.T) {
	srv, _ := newTestServer(t)
	for i := 0; i < 5; i++ {
		rec := do(srv, "POST", "/api/admin/login", `{"password":"wrong"}`, nil)
		if rec.Code != 401 {
			t.Fatalf("第 %d 次错密码应 401，得到 %d", i+1, rec.Code)
		}
	}
	rec := do(srv, "POST", "/api/admin/login", `{"password":"wrong"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次应 429，得到 %d", rec.Code)
	}
	rec = do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确密码也应 429，得到 %d", rec.Code)
	}
}

// 安全：下游错误响应不得包含渠道名/上游地址。
func TestAllChannelsFailErrorIsGeneric(t *testing.T) {
	srv, st := newTestServer(t)
	key := mustCreateKey(t, st)
	secretHost := "secret-name.example.com"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	// 用带可识别名字的渠道
	_, err := st.CreateChannel(&store.Channel{
		Name: "leaky-channel", Type: "openai", BaseURL: upstream.URL + "/" + secretHost,
		APIKey: "up-key", Models: []string{"m"}, Weight: 1, Priority: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("应 502，得到 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "leaky-channel") || strings.Contains(rec.Body.String(), secretHost) {
		t.Fatalf("错误响应泄露内部信息: %s", rec.Body.String())
	}
}
