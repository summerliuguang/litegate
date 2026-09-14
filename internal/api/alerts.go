package api

// 告警推送：数据面/治理层的关键事件（预算告警/耗尽、渠道密钥冷却、余额不足、
// 错误率突增）经 alertManager 去重后异步 POST 到配置的 webhook。支持通用 JSON
// 与 ntfy 两种格式；配置存 settings 表，管理面修改即时生效。
// 热路径只做非阻塞入队，网络失败只记日志不重试——告警丢失好过拖垮网关。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"litegate/internal/store"
)

const settingsKeyAlerts = "alerts_config"

// AlertsConfig 是告警推送配置；BudgetWarnPct/CooldownMin 为 0 时用默认值。
type AlertsConfig struct {
	Enabled       bool   `json:"enabled"`
	WebhookURL    string `json:"webhook_url"`
	Format        string `json:"format"` // json（通用）| ntfy
	Topic         string `json:"topic"`  // ntfy 的 topic（format=ntfy 时必填）
	BudgetWarnPct int    `json:"budget_warn_pct"`
	CooldownMin   int    `json:"cooldown_min"`
}

func (c *AlertsConfig) normalize() {
	if c.Format != "ntfy" {
		c.Format = "json"
	}
	if c.BudgetWarnPct <= 0 || c.BudgetWarnPct >= 100 {
		c.BudgetWarnPct = 80
	}
	if c.CooldownMin <= 0 {
		c.CooldownMin = 10
	}
}

func (c *AlertsConfig) validate() string {
	if !c.Enabled {
		return ""
	}
	if c.WebhookURL == "" {
		return "webhook_url is required when enabled"
	}
	if c.Format == "ntfy" && c.Topic == "" {
		return "topic is required for ntfy format"
	}
	return ""
}

type alertEvent struct {
	event, title, message, severity string
}

type alertManager struct {
	st     *store.Store
	client *http.Client

	mu  sync.Mutex
	cfg AlertsConfig
	// last 记录同类事件的最近发送时间，冷却期内不重复打扰
	last map[string]time.Time
	ch   chan alertEvent
}

func newAlertManager(st *store.Store) *alertManager {
	a := &alertManager{
		st:     st,
		client: &http.Client{Timeout: 8 * time.Second},
		last:   map[string]time.Time{},
		ch:     make(chan alertEvent, 64),
	}
	a.reload()
	go a.loop()
	return a
}

// reload 从 settings 表加载配置；损坏的配置按默认值处理并保留在库里待管理面覆盖。
func (a *alertManager) reload() {
	var cfg AlertsConfig
	if raw, err := a.st.GetSetting(settingsKeyAlerts); err == nil {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.normalize()
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

// Config 返回当前配置副本（管理面展示）。
func (a *alertManager) Config() AlertsConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// Save 校验并持久化配置，成功后即时生效。
func (a *alertManager) Save(cfg AlertsConfig) error {
	cfg.normalize()
	if msg := cfg.validate(); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := a.st.SetSetting(settingsKeyAlerts, string(raw)); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return nil
}

// fire 非阻塞入队一个告警事件；冷却期内同类事件直接丢弃。
// dedupeKey 为空表示不去重（如手动测试）。
func (a *alertManager) fire(dedupeKey, event, title, message, severity string) {
	a.mu.Lock()
	cfg := a.cfg
	if !cfg.Enabled {
		a.mu.Unlock()
		return
	}
	now := time.Now()
	if dedupeKey != "" {
		cool := time.Duration(cfg.CooldownMin) * time.Minute
		if t, ok := a.last[dedupeKey]; ok && now.Sub(t) < cool {
			a.mu.Unlock()
			return
		}
		a.last[dedupeKey] = now
		// 防止 dedupe 表无限增长
		if len(a.last) > 4096 {
			for k, t := range a.last {
				if now.Sub(t) > 24*time.Hour {
					delete(a.last, k)
				}
			}
		}
	}
	a.mu.Unlock()

	select {
	case a.ch <- alertEvent{event: event, title: title, message: message, severity: severity}:
	default:
		log.Printf("alerts: queue full, dropped event %q", event)
	}
}

// loop 单协程消费事件并 POST webhook。
func (a *alertManager) loop() {
	for ev := range a.ch {
		a.post(ev)
	}
}

func (a *alertManager) post(ev alertEvent) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.WebhookURL == "" {
		return
	}
	var payload any
	switch cfg.Format {
	case "ntfy":
		prio := "default"
		if ev.severity == "critical" {
			prio = "high"
		}
		payload = map[string]any{
			"topic": cfg.Topic, "title": ev.title,
			"message": ev.message, "priority": prio, "tags": []string{"robot"},
		}
	default:
		payload = map[string]any{
			"event": ev.event, "severity": ev.severity, "title": ev.title,
			"message": ev.message, "app": "litegate", "ts": time.Now().UTC().Format(time.RFC3339),
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	resp, err := a.client.Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("alerts: post %q failed: %v", ev.event, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("alerts: post %q got status %d", ev.event, resp.StatusCode)
	}
}

// ---- 事件挂点 ----

// fireBudget 在预算用量越过告警线（首次）或耗尽（每窗口首次）时告警。
// 由 keyAdmission.admit 在每次请求时调用，这里做窗口内去重。
func (a *alertManager) fireBudget(keyID int64, keyName string, windowLabel string, limitUSD, cost float64, exhausted bool) {
	if limitUSD <= 0 {
		return
	}
	cfg := a.Config()
	pct := float64(cfg.BudgetWarnPct) / 100
	if !exhausted && (cost < limitUSD*pct || cost/limitUSD >= 1) {
		return // 未到线，或已越过（交给 exhausted 分支）
	}
	kind, sev := "预算告警", "warning"
	if exhausted {
		kind, sev = "预算耗尽", "critical"
	}
	dk := fmt.Sprintf("budget_%s_%d_%s", map[bool]string{true: "out", false: "warn"}[exhausted], keyID, windowLabel)
	a.fire(dk, "budget", "LiteGate "+kind,
		fmt.Sprintf("虚拟密钥「%s」%s：已用 %s / 预算 $%s（%s 窗口）",
			keyName, kind, formatUSD(cost), formatUSD(limitUSD), windowLabel), sev)
}

// fireKeyCooldown 渠道密钥进入指数冷却时告警（仅首次进入，续期不打扰）。
func (a *alertManager) fireKeyCooldown(label string, cooldown time.Duration) {
	a.fire("key_cooldown_"+label, "key_cooldown", "LiteGate 渠道密钥冷却",
		fmt.Sprintf("%s 连续失败，冷却 %s", label, cooldown.Truncate(time.Second)), "warning")
}

// fireBalance 渠道某把密钥余额低于阈值时告警（每把密钥每天最多一次）。
func (a *alertManager) fireBalance(channelName, keyMasked string, remaining float64, currency string, below float64) {
	a.fire("balance_"+channelName+"_"+keyMasked+"_"+time.Now().Format("2006-01-02"),
		"balance_low", "LiteGate 上游余额不足",
		fmt.Sprintf("渠道「%s」密钥 %s 余额 %s，低于阈值 %g", channelName, keyMasked,
			fmtMoney(remaining, currency), below),
		"critical")
}

// fireErrorSpike 近 5 分钟错误率突增时告警（固定 30 分钟冷却）。
func (a *alertManager) fireErrorSpike(fails, total int64) {
	a.mu.Lock()
	if t, ok := a.last["error_spike"]; ok && time.Since(t) < 30*time.Minute {
		a.mu.Unlock()
		return
	}
	a.last["error_spike"] = time.Now()
	a.mu.Unlock()
	a.fire("", "error_spike", "LiteGate 错误率突增",
		fmt.Sprintf("近 5 分钟 %d 笔请求中 %d 笔失败（%.0f%%），请检查渠道健康", total, fails,
			float64(fails)/float64(total)*100), "critical")
}

// TestAlert 发送一条测试通知（管理面"测试"按钮用；不去重直接投递）。
func (a *alertManager) TestAlert() error {
	cfg := a.Config()
	if !cfg.Enabled {
		return fmt.Errorf("alerts disabled")
	}
	a.post(alertEvent{event: "test", title: "LiteGate 测试通知",
		message: "这是一条测试告警，收到即说明 webhook 配置正确", severity: "info"})
	return nil
}

// fmtMoney 按币种格式化金额（告警文案用；USD 前缀 $，其余用代码后缀标注）。
func fmtMoney(v float64, currency string) string {
	if currency == "USD" {
		return "$" + formatUSD(v)
	}
	return formatUSD(v) + " " + currency
}
