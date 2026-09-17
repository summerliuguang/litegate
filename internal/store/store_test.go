package store

// store 层单测：迁移幂等、主密钥持久化与渠道凭证加解密、按日用量账本的
// 累计/清理、审计保留策略、settings 读写。api 包的黑盒测试覆盖路由语义，
// 这里聚焦存储自身的不变量。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAt(t *testing.T, path string, secret []byte) *Store {
	t.Helper()
	s, err := Open(path, secret)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testSecret() []byte {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	return secret
}

// TestOpenTwiceIdempotent 同一库文件反复 Open：schema/migrate 不报错，
// 主密钥从 settings 持久化恢复（同一密钥才能解出旧密文）。
func TestOpenTwiceIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s1 := openAt(t, path, nil)

	cid, err := s1.CreateChannel(&Channel{
		Name: "c", Type: "openai", BaseURL: "http://x/v1",
		APIKeys: []ChannelKey{{Key: "up-secret-1"}, {Key: "up-secret-2"}}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	s1.Close()

	s2 := openAt(t, path, nil)
	c, err := s2.GetChannel(cid)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if len(c.APIKeys) != 2 || c.APIKeys[0].Key != "up-secret-1" || c.APIKeys[1].Key != "up-secret-2" {
		t.Fatalf("plaintext keys lost across reopen: %+v", c.APIKeys)
	}
	// 落库的是密文（enc:v1: 前缀），明文不直接进 channel_keys.key_enc
	var enc string
	if err := s2.DB.QueryRow(`SELECT key_enc FROM channel_keys LIMIT 1`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "enc:v1:") {
		t.Fatalf("key not encrypted at rest: %q", enc)
	}
}

// TestOpenWithExternalSecret 外置主密钥：指定 secret 时用它加解密，
// 与库内自动生成的密钥互不相通（换错密钥取不到明文）。
func TestOpenWithExternalSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s1 := openAt(t, path, testSecret())
	if _, err := s1.CreateChannel(&Channel{
		Name: "c", Type: "openai", BaseURL: "http://x/v1",
		APIKeys: []ChannelKey{{Key: "plain-key"}}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2 := openAt(t, path, testSecret())
	c, err := s2.GetChannelByName("c")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Key != "plain-key" {
		t.Fatalf("external secret roundtrip broken: %+v", c.APIKeys)
	}
	s2.Close()

	// 换一把错误的外置密钥：密文解不开必须显式报错（而不是静默返回垃圾/
	// 空密钥）——这正是启动时强制校验 LITEGATE_SECRET 格式的理由。
	wrong := append([]byte(nil), testSecret()...)
	wrong[0] ^= 0xFF
	s3 := openAt(t, path, wrong)
	if _, err := s3.GetChannelByName("c"); err == nil {
		c3, _ := s3.GetChannelByName("c")
		for _, k := range c3.APIKeys {
			if k.Key == "plain-key" {
				t.Fatal("wrong secret must never yield plaintext")
			}
		}
		t.Fatal("wrong secret read should fail loudly")
	} else if !strings.Contains(err.Error(), "cipher") && !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("unexpected error class: %v", err)
	}
}

// TestKeyUsageDayAccumulateAndPrune 账本累计与清理边界。
func TestKeyUsageDayAccumulateAndPrune(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.AddKeyUsageDay(7, now, 1, 0, 10, 0.1, 0.2))
	must(s.AddKeyUsageDay(7, now, 2, 1, 20, 0.3, 0.4)) // 同日累加
	// 40 天前的一笔（超出默认月窗口但应留在 400 天保留期内）
	must(s.AddKeyUsageDay(7, now.AddDate(0, 0, -40), 1, 0, 5, 1.0, 0))

	cu, cc, tok, err := s.KeyUsageSince(7, "2000-01-01")
	must(err)
	if cu < 1.4-1e-9 || cc < 0.6-1e-9 || tok != 35 {
		t.Fatalf("since all = %v/%v/%v, want 1.4/0.6/35", cu, cc, tok)
	}
	cu, _, tok, err = s.KeyUsageSince(7, now.UTC().AddDate(0, 0, -6).Format("2006-01-02"))
	must(err)
	if cu < 0.4-1e-9 || tok != 30 {
		t.Fatalf("since 7d = %v/%v, want 0.4/30", cu, tok)
	}

	// 清理 30 天前：40 天前那笔消失，今日保留
	if n, err := s.PruneKeyUsageDays(30); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1, nil", n, err)
	}
	cu, _, tok, err = s.KeyUsageSince(7, "2000-01-01")
	must(err)
	if cu < 0.4-1e-9 || tok != 30 {
		t.Fatalf("after prune = %v/%v, want 0.4/30", cu, tok)
	}
	if n, err := s.PruneKeyUsageDays(0); err != nil || n != 0 {
		t.Fatalf("prune disabled = %d, %v", n, err)
	}
}

// TestAuditKeepAndPrune 审计保留条数可配：超限自动清理最旧。
func TestAuditKeepAndPrune(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if err := s.SetAuditKeep(120); err != nil {
		t.Fatal(err)
	}
	// 缓存生效路径
	if k := s.AuditKeep(); k != 120 {
		t.Fatalf("AuditKeep = %d, want 120", k)
	}
	for i := 0; i < 150; i++ {
		if err := s.InsertAudit("t", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 120 || n > 121 { // 清理滞后于最新一条一步，允许 1 条浮动
		t.Fatalf("audit rows = %d, want ~120", n)
	}
	// 钳制边界
	if err := s.SetAuditKeep(1); err != nil {
		t.Fatal(err)
	}
	if k := s.AuditKeep(); k != auditKeepMin {
		t.Fatalf("clamped keep = %d, want %d", k, auditKeepMin)
	}
}

// TestSettingsRoundtrip settings 的读写与缺失语义。
func TestSettingsRoundtrip(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	if _, err := s.GetSetting("nope"); err != ErrNotFound {
		t.Fatalf("missing setting err = %v, want ErrNotFound", err)
	}
	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k", "v2"); err != nil { // upsert 覆盖
		t.Fatal(err)
	}
	if v, err := s.GetSetting("k"); err != nil || v != "v2" {
		t.Fatalf("get = %q, %v; want v2", v, err)
	}
}

// TestKeyUsageDayBackfillStore 账本回填（store 侧）：从日志聚合、按价格表
// 拆币种、二次执行幂等。
func TestKeyUsageDayBackfillStore(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "s.db"), nil)
	ak := &APIKey{Name: "t"}
	if err := s.CreateAPIKey(ak); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertModelPrice(&ModelPrice{Model: "m-cny", InputPrice: 1, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
	logs := []*RequestLog{
		{APIKeyID: ak.ID, Model: "m-cny", Status: 200, PromptTokens: 5, CostUSD: 2.0},
		{APIKeyID: ak.ID, Model: "m-usd", Status: 200, PromptTokens: 7, CostUSD: 0.5},
	}
	for _, l := range logs {
		if err := s.InsertRequestLog(l); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.BackfillKeyUsageFromLogs()
	if err != nil || n != 1 {
		t.Fatalf("backfill = %d, %v; want 1 group", n, err)
	}
	rows, err := s.KeyDailyUsage(ak.ID, 7)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	r := rows[0]
	if r.Requests != 2 || r.Tokens != 12 || r.CostCNY < 2.0-1e-9 || r.CostUSD < 0.5-1e-9 {
		t.Fatalf("row = %+v, want 2 req / 12 tok / ¥2 / $0.5", r)
	}
	if n, err = s.BackfillKeyUsageFromLogs(); err != nil || n != 0 {
		t.Fatalf("second backfill = %d, %v; want 0 (idempotent)", n, err)
	}
}
