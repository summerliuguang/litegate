package api

// M5 可观测与运维的回归测试：延迟分位/实时指标/按应用分摊、X-LiteGate-App 归因、
// 配置导出导入、模型自动发现、备份、日志保留、深度 healthz。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"litegate/internal/store"
)

func TestDashboardLatencyAndApp(t *testing.T) {
	srv, st := newTestServer(t)
	insertLog := func(latency, ttfb, completion int64, app string, cost float64) {
		err := st.InsertRequestLog(&store.RequestLog{
			Model: "m", Protocol: "openai", App: app, Status: 200,
			LatencyMs: latency, TtfbMs: ttfb, CompletionTokens: completion, CostUSD: cost,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insertLog(100, 50, 100, "hermes", 0.1)
	insertLog(200, 100, 200, "hermes", 0.2)
	insertLog(1000, 500, 500, "codex", 0.3)
	insertLog(5000, 0, 0, "", 0) // 失败样本不进分位？状态 200 也计入；用 error 字段排除
	if _, err := st.DB.Exec(`UPDATE request_logs SET error = 'boom' WHERE latency_ms = 5000`); err != nil {
		t.Fatal(err)
	}

	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	out := do(srv, "GET", "/api/admin/dashboard", "",
		map[string]string{"Authorization": "Bearer " + login.Token})
	var d struct {
		LatencyP50Ms int64  `json:"latency_p50_ms"`
		LatencyP95Ms int64  `json:"latency_p95_ms"`
		AvgTps       float64 `json:"avg_tps"`
		ByApp        []struct {
			App      string  `json:"app"`
			Requests int64   `json:"requests"`
			CostUSD  float64 `json:"cost_usd"`
		} `json:"by_app"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	// 成功样本延迟 [100,200,1000]：P50=200，P95=1000
	if d.LatencyP50Ms != 200 || d.LatencyP95Ms != 1000 {
		t.Fatalf("p50=%d p95=%d, want 200/1000", d.LatencyP50Ms, d.LatencyP95Ms)
	}
	// 生成速度 = (100+200+500) tok / ((50+100+500)ms) = 800/0.65s ≈ 1230.8
	if d.AvgTps < 1200 || d.AvgTps > 1250 {
		t.Fatalf("avg_tps = %v", d.AvgTps)
	}
	if len(d.ByApp) != 3 {
		t.Fatalf("by_app = %+v, want 3 组（hermes/codex/未标注）", d.ByApp)
	}
	if d.ByApp[0].App != "codex" && d.ByApp[0].App != "hermes" {
		t.Fatalf("by_app 排序异常: %+v", d.ByApp)
	}
	for _, a := range d.ByApp {
		if a.App == "hermes" && (a.Requests != 2 || a.CostUSD != 0.3) {
			t.Fatalf("hermes 分摊 = %+v", a)
		}
	}
}

func TestAppAttributionEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key, "X-LiteGate-App": "my-agent"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	page, err := st.ListLogs(store.LogFilter{App: "my-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].App != "my-agent" {
		t.Fatalf("app attribution failed: %+v", page.Items)
	}
	// 不带头时不归因
	rec = do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	page, _ = st.ListLogs(store.LogFilter{App: "my-agent"})
	if len(page.Items) != 1 {
		t.Fatalf("无头请求不应带 app，共 %d 条", len(page.Items))
	}
}

func TestConfigExportImportRoundtrip(t *testing.T) {
	// 源实例：渠道（2 把 key）+ 价格
	srvSrc, stSrc := newTestServer(t)
	_, err := stSrc.CreateChannel(&store.Channel{
		Name: "exp-ch", Type: "openai", BaseURL: "http://upstream.example/v1",
		APIKeys: []store.ChannelKey{{Key: "kk-1"}, {Key: "kk-2"}},
		Models:  []string{"m"}, ModelMap: map[string]string{"fast": "m"}, Weight: 2, Priority: 5, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stSrc.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 1, OutputPrice: 2}); err != nil {
		t.Fatal(err)
	}

	rec := do(srvSrc, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	hdr := map[string]string{"Authorization": "Bearer " + login.Token}

	// 不带 include_keys：不能出现明文 key
	out := do(srvSrc, "GET", "/api/admin/config/export", "", hdr)
	if strings.Contains(out.Body.String(), "kk-1") {
		t.Fatalf("默认导出泄露明文密钥: %s", out.Body.String())
	}
	// 带 include_keys：导出明文供迁移
	out = do(srvSrc, "GET", "/api/admin/config/export?include_keys=1", "", hdr)
	if !strings.Contains(out.Body.String(), "kk-1") || !strings.Contains(out.Body.String(), "kk-2") {
		t.Fatalf("include_keys 导出缺明文: %s", out.Body.String())
	}

	// 目标实例导入
	srvDst, stDst := newTestServer(t)
	rec = do(srvDst, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login2 struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login2)
	rec = do(srvDst, "POST", "/api/admin/config/import", out.Body.String(),
		map[string]string{"Authorization": "Bearer " + login2.Token, "Content-Type": "application/json"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"channels_upserted":1`) {
		t.Fatalf("import = %d %s", rec.Code, rec.Body.String())
	}

	ch, err := stDst.GetChannelByName("exp-ch")
	if err != nil {
		t.Fatalf("导入的渠道不存在: %v", err)
	}
	if len(ch.APIKeys) != 2 || ch.ModelMap["fast"] != "m" || ch.Priority != 5 {
		t.Fatalf("导入渠道字段不完整: %+v", ch)
	}
	if ch.APIKeys[0].Key != "kk-1" && ch.APIKeys[1].Key != "kk-1" {
		t.Fatalf("导入后密钥明文不符: %+v", ch.APIKeys)
	}
	p, err := stDst.ListModelPrices()
	if err != nil || len(p) != 1 || p[0].Model != "m" || p[0].InputPrice != 1 {
		t.Fatalf("导入价格不符: %+v err=%v", p, err)
	}
}

func TestDiscoverChannelModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer up-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"m-b"},{"id":"m-a"}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	id := mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	out := do(srv, "GET", "/api/admin/channels/"+fmt.Sprintf("%d", id)+"/discover", "",
		map[string]string{"Authorization": "Bearer " + login.Token})
	var r struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || len(r.Models) != 2 || r.Models[0] != "m-a" || r.Models[1] != "m-b" {
		t.Fatalf("discover = %+v", r)
	}
}

func TestBackupAndList(t *testing.T) {
	srv, st := newTestServer(t)
	key := mustCreateKey(t, st)
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	hdr := map[string]string{"Authorization": "Bearer " + login.Token}

	out := do(srv, "POST", "/api/admin/db/backup", "", hdr)
	if out.Code != http.StatusOK {
		t.Fatalf("backup = %d %s", out.Code, out.Body.String())
	}
	var b store.BackupInfo
	if err := json.Unmarshal(out.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.File, "litegate-") || b.Size <= 0 {
		t.Fatalf("backup info = %+v", b)
	}
	out = do(srv, "GET", "/api/admin/db/backups", "", hdr)
	if !strings.Contains(out.Body.String(), b.File) {
		t.Fatalf("backups list missing %s: %s", b.File, out.Body.String())
	}
	_ = key
}

func TestPruneLogs(t *testing.T) {
	_, st := newTestServer(t)
	if err := st.InsertRequestLog(&store.RequestLog{Model: "old", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRequestLog(&store.RequestLog{Model: "new", Status: 200}); err != nil {
		t.Fatal(err)
	}
	// 把一条挪到 10 天前
	if _, err := st.DB.Exec(`UPDATE request_logs SET ts = datetime('now', '-10 days') WHERE model = 'old'`); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneLogs(7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	page, _ := st.ListLogs(store.LogFilter{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].Model != "new" {
		t.Fatalf("remaining = %+v", page.Items)
	}
}

func TestDeepHealthz(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, "GET", "/healthz?deep=1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("deep healthz = %d", rec.Code)
	}
	var h struct {
		Status        string `json:"status"`
		Version       string `json:"version"`
		Db            string `json:"db"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Db != "ok" || h.Version == "" || h.UptimeSeconds < 0 {
		t.Fatalf("deep healthz = %+v", h)
	}
	// 普通 healthz 保持轻量形状
	rec = do(srv, "GET", "/healthz", "", nil)
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) || strings.Contains(rec.Body.String(), "version") {
		t.Fatalf("plain healthz changed: %s", rec.Body.String())
	}
}
