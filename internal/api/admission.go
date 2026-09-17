package api

// 虚拟密钥准入控制：过期时间、RPM/TPM 滑动窗口限速、日/月预算（美元或 token）。
// 全部状态在单进程内存：限速窗口天然易失；预算在启动时从日志表回填、请求完成后
// 增量累计，重启不丢账。窗口按 UTC 天/月对齐（与日志表的 ts 存储口径一致）。

import (
	"fmt"
	"sync"
	"time"

	"litegate/internal/store"
)

type tokPoint struct {
	ts     time.Time
	tokens int64
}

type windowUse struct {
	label  string // "2026-09-08"（日）或 "2026-09"（月），变更即滚动重置
	cost   float64
	tokens int64
}

type keyAdmission struct {
	mu     sync.Mutex
	alerts *alertManager
	rpm    map[int64][]time.Time
	tpm    map[int64][]tokPoint
	day    map[int64]*windowUse
	month  map[int64]*windowUse
	cur    map[int64]int64 // 密钥级并发在请求数（ConcurrencyLimit>0 时维护）
}

func newKeyAdmission(alerts *alertManager) *keyAdmission {
	return &keyAdmission{
		alerts: alerts,
		rpm:    map[int64][]time.Time{},
		tpm:    map[int64][]tokPoint{},
		day:    map[int64]*windowUse{},
		month:  map[int64]*windowUse{},
		cur:    map[int64]int64{},
	}
}

// load 启动时从日志表回填各密钥本日/本月用量。
func (a *keyAdmission) load(st *store.Store) {
	keys, err := st.ListAPIKeys()
	if err != nil {
		return
	}
	dayStart := time.Now().UTC().Format("2006-01-02") + " 00:00:00"
	monthStart := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	for i := range keys {
		k := &keys[i]
		if c, t, err := st.UsageSince(k.ID, dayStart); err == nil {
			a.day[k.ID] = &windowUse{label: dayLabel(), cost: c, tokens: t}
		}
		if c, t, err := st.UsageSince(k.ID, monthStart); err == nil {
			a.month[k.ID] = &windowUse{label: monthLabel(), cost: c, tokens: t}
		}
	}
}

// enter 密钥并发上限准入：返回释放函数与是否放行。limit<=0 恒放行零开销。
// 须在 admit 通过后调用，释放用 defer。
func (a *keyAdmission) enter(ak *store.APIKey) (release func(), ok bool) {
	if ak.ConcurrencyLimit <= 0 {
		return func() {}, true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur[ak.ID] >= ak.ConcurrencyLimit {
		return nil, false
	}
	a.cur[ak.ID]++
	return func() {
		a.mu.Lock()
		a.cur[ak.ID]--
		a.mu.Unlock()
	}, true
}

func dayLabel() string   { return time.Now().UTC().Format("2006-01-02") }
func monthLabel() string { return time.Now().UTC().Format("2006-01") }

// admit 返回拒绝时应使用的 HTTP 状态码与原因；0 表示放行。
func (a *keyAdmission) admit(ak *store.APIKey) (int, string) {
	now := time.Now()
	// 过期时间：ExpiresAt 当日（本地时区）结束后失效
	if ak.ExpiresAt != "" {
		if t, err := time.ParseInLocation("2006-01-02", ak.ExpiresAt, time.Local); err == nil {
			if now.After(t.Add(24 * time.Hour)) {
				return 401, "api key expired on " + ak.ExpiresAt
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ak.RPMLimit > 0 {
		q := pruneTimes(a.rpm[ak.ID], now, time.Minute)
		if int64(len(q)) >= ak.RPMLimit {
			return 429, "rate limited: rpm limit reached for this api key"
		}
		q = append(q, now)
		a.rpm[ak.ID] = q
	}
	if ak.TPMLimit > 0 {
		q := pruneTok(a.tpm[ak.ID], now, time.Minute)
		var sum int64
		for _, p := range q {
			sum += p.tokens
		}
		if sum >= ak.TPMLimit {
			return 429, "rate limited: tpm limit reached for this api key"
		}
		a.tpm[ak.ID] = q
	}
	if ak.BudgetUSD > 0 || ak.BudgetTokens > 0 {
		use := a.budgetWindow(ak)
		if ak.BudgetUSD > 0 {
			// 预算越过告警线（含耗尽）时推送告警；fireBudget 内部做窗口去重
			if a.alerts != nil {
				a.alerts.fireBudget(ak.ID, ak.Name, use.label, ak.BudgetUSD, use.cost, use.cost >= ak.BudgetUSD)
			}
			if use.cost >= ak.BudgetUSD {
				return 429, "budget exhausted: " + ak.BudgetPeriod + " budget $" +
					formatUSD(ak.BudgetUSD) + " reached for this api key"
			}
		}
		if ak.BudgetTokens > 0 && use.tokens >= ak.BudgetTokens {
			return 429, "budget exhausted: " + ak.BudgetPeriod + " token budget reached for this api key"
		}
	}
	return 0, ""
}

// budgetWindow 取预算对应窗口（daily/monthly），标签过期自动清零。
func (a *keyAdmission) budgetWindow(ak *store.APIKey) *windowUse {
	if ak.BudgetPeriod == "monthly" {
		w := a.month[ak.ID]
		if w == nil || w.label != monthLabel() {
			w = &windowUse{label: monthLabel()}
			a.month[ak.ID] = w
		}
		return w
	}
	w := a.day[ak.ID]
	if w == nil || w.label != dayLabel() {
		w = &windowUse{label: dayLabel()}
		a.day[ak.ID] = w
	}
	return w
}

// record 请求完成后累计 TPM 与预算用量；未启用任何限额时零开销直接返回。
func (a *keyAdmission) record(ak *store.APIKey, prompt, completion int64, cost float64) {
	if ak == nil {
		return
	}
	if ak.TPMLimit == 0 && ak.BudgetUSD == 0 && ak.BudgetTokens == 0 {
		return
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if ak.TPMLimit > 0 {
		a.tpm[ak.ID] = append(pruneTok(a.tpm[ak.ID], now, time.Minute),
			tokPoint{ts: now, tokens: prompt + completion})
	}
	if ak.BudgetUSD > 0 || ak.BudgetTokens > 0 {
		w := a.budgetWindow(ak)
		w.cost += cost
		w.tokens += prompt + completion
	}
}

// budgetUsage 返回密钥当前预算窗口的已用金额与 token 数——与拦截判定同口径
// （内存累计 + 启动时从日志回填），供管理台画预算消耗进度条。未配预算返回 0。
func (a *keyAdmission) budgetUsage(ak *store.APIKey) (float64, int64) {
	if ak.BudgetUSD == 0 && ak.BudgetTokens == 0 {
		return 0, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.budgetWindow(ak)
	return w.cost, w.tokens
}

func pruneTimes(q []time.Time, now time.Time, win time.Duration) []time.Time {
	out := q[:0]
	for _, t := range q {
		if now.Sub(t) < win {
			out = append(out, t)
		}
	}
	return out
}

func pruneTok(q []tokPoint, now time.Time, win time.Duration) []tokPoint {
	out := q[:0]
	for _, p := range q {
		if now.Sub(p.ts) < win {
			out = append(out, p)
		}
	}
	return out
}

func formatUSD(v float64) string {
	return fmt.Sprintf("%g", v)
}
