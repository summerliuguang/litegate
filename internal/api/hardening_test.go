package api

// 安全加固与稳定性补充项的测试：SSO 回跳 Host 白名单、明文密钥导出二次确认
// 与审计、Prometheus 标签转义、上游空闲看门狗、深度健康检查、密钥按天用量。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"litegate/internal/store"
)

func newTestStoreOnly(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestSSORedirectHostAllowlist 回跳 Host 只从白名单取：白名单内原样回跳，
// 白名单外（伪造 Host）回退白名单第一项，杜绝开放重定向。
func TestSSORedirectHostAllowlist(t *testing.T) {
	srv := NewServer(newTestStoreOnly(t), "pw", nil, []string{"panel.example:8443", "192.168.5.15:29007"})

	req := httptest.NewRequest("GET", "/api/admin/sso-redirect", nil)
	req.Host = "panel.example:8443"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "back=http") || !strings.Contains(loc, "panel.example%3A8443") {
		t.Fatalf("allowed host not preserved: %q", loc)
	}

	req2 := httptest.NewRequest("GET", "/api/admin/sso-redirect", nil)
	req2.Host = "evil.example"
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	loc2 := rec2.Header().Get("Location")
	if strings.Contains(loc2, "evil.example") {
		t.Fatalf("hostile host leaked into redirect: %q", loc2)
	}
	if !strings.Contains(loc2, "panel.example%3A8443") {
		t.Fatalf("fallback to first allowlist entry expected: %q", loc2)
	}
}

// TestSSORedirectDefaultHosts 未配置白名单时使用内置默认（局域网入口 + 本机直连）。
func TestSSORedirectDefaultHosts(t *testing.T) {
	srv := NewServer(newTestStoreOnly(t), "pw", nil, nil)
	req := httptest.NewRequest("GET", "/api/admin/sso-redirect", nil)
	req.Host = "192.168.5.15:29007"
	req.Header.Set("X-Forwarded-Proto", "https") // 经 nginx 反代的形态
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "back=https%3A%2F%2F192.168.5.15%3A29007%2F") {
		t.Fatalf("default lan host not preserved: %q", rec.Header().Get("Location"))
	}
}

// TestExportIncludeKeysConfirmation include_keys=1 导出明文需要 X-Admin-Password
// 二次确认（缺失/错误 403），确认后 200 且记审计。
func TestExportIncludeKeysConfirmation(t *testing.T) {
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", "https://up.example/v1", []string{"m1"}, 1)
	tok := adminToken(t, srv)
	h := map[string]string{"Authorization": "Bearer " + tok}

	rec := do(srv, "GET", "/api/admin/config/export?include_keys=1", "", h)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing confirm header: status = %d, want 403", rec.Code)
	}

	rec = do(srv, "GET", "/api/admin/config/export?include_keys=1", "",
		map[string]string{"Authorization": "Bearer " + tok, "X-Admin-Password": "wrong"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong confirm password: status = %d, want 403", rec.Code)
	}

	rec = do(srv, "GET", "/api/admin/config/export?include_keys=1", "",
		map[string]string{"Authorization": "Bearer " + tok, "X-Admin-Password": "testpw"})
	if rec.Code != http.StatusOK {
		t.Fatalf("correct confirm: status = %d, want 200", rec.Code)
	}
	var out struct {
		Channels []struct {
			APIKeys []string `json:"api_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(out.Channels) == 0 || len(out.Channels[0].APIKeys) == 0 {
		t.Fatal("expected plaintext api keys in confirmed export")
	}

	auditOK := false
	rows, err := st.ListAudit(50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, a := range rows {
		if a.Action == "config.export" && strings.Contains(a.Detail, "明文密钥=1") {
			auditOK = true
		}
	}
	if !auditOK {
		t.Fatal("config.export with plaintext keys not audited")
	}

	// 不带 include_keys：无需确认头，照常导出（只有打码值）
	rec = do(srv, "GET", "/api/admin/config/export", "", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain export status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"api_keys":[{`) || strings.Contains(rec.Body.String(), `"api_keys":["sk-`) {
		t.Fatal("plain export must not contain plaintext keys")
	}
}

// TestPromEscape 按 Prometheus 规范只转义 \\ \" \n 三种。
func TestPromEscape(t *testing.T) {
	cases := [][2]string{
		{"deepseek-flash", `deepseek-flash`},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{"mix\\\n\"", `mix\\` + `\n` + `\"`},
	}
	for _, c := range cases {
		if got := promEscape(c[0]); got != c[1] {
			t.Errorf("promEscape(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// TestUpstreamIdleWatchdog 上游吐完响应头后挂住不吐正文：空闲看门狗必须在
// idle 超时后中断阻塞中的 Body.Read。
func TestUpstreamIdleWatchdog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 挂住：客户端断开（看门狗触发）时退出
	}))
	defer up.Close()

	p := &proxy{st: newTestStoreOnly(t), client: newUpstreamClient(), idleTimeout: 200 * time.Millisecond}
	c := &store.Channel{Name: "up", Type: "openai", BaseURL: up.URL, Enabled: true}
	req := httptest.NewRequest("POST", "http://gw/v1/chat/completions", nil)

	resp, err := p.attemptUpstream(req, c, "k", "/chat/completions", []byte("{}"))
	if err != nil {
		t.Fatalf("attemptUpstream: %v", err)
	}
	defer resp.Body.Close()

	done := make(chan error, 1)
	go func() {
		_, err = io.ReadAll(resp.Body)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected read error after idle timeout, got clean EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not abort stalled upstream body within 5s")
	}
}

// TestWatchdogDisabled 空闲上限为 0 时挂 watchdogBody 也能正常读（timer 为 nil）。
func TestWatchdogDisabled(t *testing.T) {
	b := &watchdogBody{ReadCloser: io.NopCloser(strings.NewReader("hello")),
		cancel: context.CancelFunc(func() {})}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestHealthzDeep 深度检查覆盖读/写/加解密三项并返回 ok。
func TestHealthzDeep(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, "GET", "/healthz?deep=1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"status", "db_read", "db_write", "crypto", "version"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("deep healthz missing %q: %v", k, out)
		}
	}
	if out["db_write"] != "ok" || out["crypto"] != "ok" {
		t.Fatalf("deep checks not ok: %v", out)
	}
}

// TestKeyDailyUsage 按天聚合：请求数、失败数、token 合计。
func TestKeyDailyUsage(t *testing.T) {
	st := newTestStoreOnly(t)
	ak := &store.APIKey{Name: "t"}
	if err := st.CreateAPIKey(ak); err != nil {
		t.Fatalf("create key: %v", err)
	}
	logs := []*store.RequestLog{
		{APIKeyID: ak.ID, Model: "m", Protocol: "openai", Status: 200, PromptTokens: 10, CompletionTokens: 5},
		{APIKeyID: ak.ID, Model: "m", Protocol: "openai", Status: 500, Error: "boom", PromptTokens: 7},
	}
	for _, l := range logs {
		if err := st.InsertRequestLog(l); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
	rows, err := st.KeyDailyUsage(ak.ID, 7)
	if err != nil {
		t.Fatalf("KeyDailyUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (today)", len(rows))
	}
	r := rows[0]
	if r.Requests != 2 || r.Errors != 1 || r.Tokens != 22 {
		t.Fatalf("row = %+v, want requests=2 errors=1 tokens=22", r)
	}
	// 其他密钥的日志不计入
	rows, err = st.KeyDailyUsage(ak.ID+100, 7)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unrelated key rows = %v, err = %v", rows, err)
	}
}

// TestLogsFilterByID 日志支持 id 精确过滤。
func TestLogsFilterByID(t *testing.T) {
	st := newTestStoreOnly(t)
	l1 := &store.RequestLog{Model: "a", Protocol: "openai", Status: 200}
	l2 := &store.RequestLog{Model: "b", Protocol: "openai", Status: 200}
	if err := st.InsertRequestLog(l1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertRequestLog(l2); err != nil {
		t.Fatalf("insert: %v", err)
	}
	page, err := st.ListLogs(store.LogFilter{ID: l2.ID})
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != l2.ID {
		t.Fatalf("page = %+v, want single log %d", page, l2.ID)
	}
}
