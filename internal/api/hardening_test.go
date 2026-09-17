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

// TestKeyDailyUsage 按天聚合（key_usage_day 账本）：请求数、失败数、token、双币种成本。
func TestKeyDailyUsage(t *testing.T) {
	st := newTestStoreOnly(t)
	ak := &store.APIKey{Name: "t"}
	if err := st.CreateAPIKey(ak); err != nil {
		t.Fatalf("create key: %v", err)
	}
	now := time.Now()
	if err := st.AddKeyUsageDay(ak.ID, now, 1, 0, 15, 0.004, 0); err != nil {
		t.Fatalf("add usage: %v", err)
	}
	if err := st.AddKeyUsageDay(ak.ID, now, 1, 1, 7, 0, 0.5); err != nil {
		t.Fatalf("add usage: %v", err)
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
	if r.CostUSD < 0.004-1e-9 || r.CostCNY < 0.5-1e-9 {
		t.Fatalf("cost split = %v/%v, want 0.004/0.5", r.CostUSD, r.CostCNY)
	}
	// 其他密钥的用量不计入
	rows, err = st.KeyDailyUsage(ak.ID+100, 7)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unrelated key rows = %v, err = %v", rows, err)
	}
}

// TestKeyUsageDayBackfill 日志 → 账本的一次性聚合：币种按价格表判定，
// 重复执行幂等；账本独立于日志存在（模拟日志已被清理后仍可回填出完整窗口）。
func TestKeyUsageDayBackfill(t *testing.T) {
	st := newTestStoreOnly(t)
	ak := &store.APIKey{Name: "t"}
	if err := st.CreateAPIKey(ak); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertModelPrice(&store.ModelPrice{Model: "cny-m", InputPrice: 2, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertModelPrice(&store.ModelPrice{Model: "usd-m", InputPrice: 2}); err != nil {
		t.Fatal(err)
	}
	logs := []*store.RequestLog{
		{APIKeyID: ak.ID, Model: "cny-m", Status: 200, PromptTokens: 10, CostUSD: 3.6},
		{APIKeyID: ak.ID, Model: "usd-m", Status: 200, PromptTokens: 5, CostUSD: 0.4},
		{APIKeyID: ak.ID, Model: "usd-m", Status: 500, Error: "boom", PromptTokens: 1},
	}
	for _, l := range logs {
		if err := st.InsertRequestLog(l); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.BackfillKeyUsageFromLogs()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n < 1 {
		t.Fatalf("backfilled groups = %d, want >= 1", n)
	}
	cu, cc, tok, err := st.KeyUsageSince(ak.ID, "2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if cu < 0.4-1e-9 || cc < 3.6-1e-9 || tok != 16 {
		t.Fatalf("since = %v/%v/%v, want 0.4/3.6/16", cu, cc, tok)
	}
	// 幂等：已有数据时不再累计
	n2, err := st.BackfillKeyUsageFromLogs()
	if err != nil || n2 != 0 {
		t.Fatalf("second backfill = %d, %v; want 0, nil", n2, err)
	}
	// 账本读数不受日志清理影响（模拟月预算 × 短日志保留期的旧缺陷场景）
	if _, err := st.DB.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	cu, cc, tok, err = st.KeyUsageSince(ak.ID, "2000-01-01")
	if err != nil || cu < 0.4-1e-9 || tok != 16 {
		t.Fatalf("after logs pruned: %v/%v/%v err=%v; usage ledger must survive", cu, cc, tok, err)
	}
}

// TestBudgetCurrencyNorm 预算判定按 usd_cny_rate 归一：¥ 计价成本不再被
// 直接当美元扣，混合币种与纯 CNY 消费都能在正确的点拦截。
func TestBudgetCurrencyNorm(t *testing.T) {
	st := newTestStoreOnly(t)
	ak := &store.APIKey{Name: "t", BudgetUSD: 1, BudgetPeriod: "daily"}
	if err := st.CreateAPIKey(ak); err != nil {
		t.Fatal(err)
	}
	a := newKeyAdmission(nil)
	a.load(st)

	a.record(ak, 0, 0, 0, 3.6) // ¥3.6 → 默认汇率 7.2 → $0.5
	if status, _ := a.admit(ak); status != 0 {
		t.Fatalf("should allow under budget, got %d", status)
	}
	usd, cny, tok := a.budgetUsage(ak)
	if usd < 0.5-1e-9 || cny < 3.6-1e-9 || tok != 0 {
		t.Fatalf("budgetUsage = %v/%v/%v, want 0.5/3.6/0", usd, cny, tok)
	}
	a.record(ak, 0, 0, 0.5, 0) // 累计 $1.0 等值 → 达到预算
	if status, msg := a.admit(ak); status != 429 {
		t.Fatalf("should block at $1 equivalent, got %d (%s)", status, msg)
	}
	// 汇率改 3.6 后，同样一笔 ¥3.6 折 $1.0 → 单笔即达预算被拦（旧口径要 7 倍消费才拦）
	if err := st.SetSetting("usd_cny_rate", "3.6"); err != nil {
		t.Fatal(err)
	}
	a.invalidateRate()
	if r := a.usdCnyRate(); r != 3.6 {
		t.Fatalf("rate = %v, want 3.6", r)
	}
	a3 := newKeyAdmission(nil)
	a3.load(st) // load 注入 st（汇率读 settings）；账本为空 → 窗口从零开始
	a3.record(ak, 0, 0, 0, 3.6)
	if status, _ := a3.admit(ak); status != 429 {
		t.Fatalf("¥3.6 at rate 3.6 should reach $1 budget, got %d", status)
	}
	// 重启回填路径：新实例从账本重建窗口（本测试未落账本 → 0，应放行）
	a2 := newKeyAdmission(nil)
	a2.load(st)
	if status, _ := a2.admit(ak); status != 0 {
		t.Fatalf("fresh instance with empty ledger should allow, got %d", status)
	}
}

// TestBudgetConfigAPI 预算汇率配置端点：默认值、更新、非法值拒绝、审计。
func TestBudgetConfigAPI(t *testing.T) {
	srv, st := newTestServer(t)
	tok := adminToken(t, srv)
	h := map[string]string{"Authorization": "Bearer " + tok}

	rec := do(srv, "GET", "/api/admin/budget", "", h)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "7.2") {
		t.Fatalf("default rate = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "PUT", "/api/admin/budget", `{"usd_cny_rate":6.5}`, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(srv, "GET", "/api/admin/budget", "", h)
	if !strings.Contains(rec.Body.String(), "6.5") {
		t.Fatalf("updated rate missing: %s", rec.Body.String())
	}
	rec = do(srv, "PUT", "/api/admin/budget", `{"usd_cny_rate":-1}`, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid rate = %d, want 400", rec.Code)
	}
	auditOK := false
	rows, _ := st.ListAudit(50)
	for _, a := range rows {
		if a.Action == "budget.update" && strings.Contains(a.Detail, "6.5") {
			auditOK = true
		}
	}
	if !auditOK {
		t.Fatal("budget.update not audited")
	}
}

// TestStreamErrorEvent 流中途断掉时，网关应补发客户端可识别的错误终止事件
// （OpenAI 协议含 "upstream stream interrupted" 与 [DONE]），与正常截断可区分。
func TestStreamErrorEvent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"a"}}]}`+"\n\n")
		f.Flush()
		conn, _, _ := w.(http.Hijacker).Hijack() // 原始断开：模拟上游中途挂掉
		conn.Close()
		<-r.Context().Done()
	}))
	defer up.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)
	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if !strings.Contains(rec.Body.String(), "upstream stream interrupted") {
		t.Fatalf("mid-stream error event missing: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("terminal [DONE] missing: %q", rec.Body.String())
	}
}

// TestKeyCooldownPersistence 冷却状态跨重启：进冷却落快照，新实例恢复未到期
// 的冷却；恢复后成功即复位并清掉快照。
func TestKeyCooldownPersistence(t *testing.T) {
	st := newTestStoreOnly(t)
	m := newKeyHealthManager(st, nil)
	m.reportFailure(1, 2, "") // 未达阈值：不进冷却、不落快照
	if v, _ := st.GetSetting("key_cooldowns"); v != "" {
		t.Fatalf("below threshold should not persist, got %q", v)
	}
	m.reportFailure(1, 2, "")
	m.reportFailure(1, 2, "") // 第 3 次：进入冷却并落快照
	v, err := st.GetSetting("key_cooldowns")
	if err != nil || !strings.Contains(v, `"1/2"`) {
		t.Fatalf("cooldown snapshot missing: %v %q", err, v)
	}

	// 模拟重启：新实例从快照恢复，该密钥应仍处冷却（available 归入冷却组）
	m2 := newKeyHealthManager(st, nil)
	m2.restore(st)
	if n := m2.coolingCount(); n != 1 {
		t.Fatalf("restored cooling count = %d, want 1", n)
	}

	// 恢复后成功：复位并清理快照
	m2.reportSuccess(1, 2)
	if v, _ := st.GetSetting("key_cooldowns"); v != "" && strings.Contains(v, `"1/2"`) {
		t.Fatalf("snapshot should be cleared after success: %q", v)
	}
	if n := m2.coolingCount(); n != 0 {
		t.Fatalf("cooling after success = %d, want 0", n)
	}
}

// TestLogsKeyIndex 按密钥过滤应命中专用索引（随用量增长的前置防线）。
func TestLogsKeyIndex(t *testing.T) {
	st := newTestStoreOnly(t)
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT * FROM request_logs WHERE api_key_id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var a, b, c, detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "idx_logs_key") {
			found = true
		}
	}
	if !found {
		t.Fatal("api_key_id filter does not use idx_logs_key")
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
