package api

// 虚拟密钥准入控制：过期时间、RPM/TPM 滑动窗口限速、日/月预算（美元或 token）。
// 预算状态在单进程内存：启动时从 key_usage_day 账本回填（账本独立于日志保留期，
// 月预算跨重启不漏账）、请求完成后增量累计，重启不丢账。窗口按 UTC 天/月对齐。
// 成本按价格表币种分列累计（costUSD/costCNY），预算判定按 usd_cny_rate 归一为
// 美元等值——混用 ¥/$ 计价模型时不再把 ¥ 数字直接当美元扣。

import (
	"strconv"
	"sync"
	"time"

	"litegate/internal/store"
)

// defaultUSDCNYRate 是未配置时的预算归一汇率；只影响预算拦截与进度条，
// 成本展示仍按各行币种拆分（¥/$ 不互加）。设置页可改（settings 表 usd_cny_rate）。
const defaultUSDCNYRate = 7.2

type tokPoint struct {
	ts     time.Time
	tokens int64
}

type windowUse struct {
	label   string // "2026-09-08"（日）或 "2026-09"（月），变更即滚动重置
	costUSD float64
	costCNY float64
	tokens  int64
}

// usdEquiv 把双币种累计归一为美元等值（预算判定的唯一口径）。
func (w *windowUse) usdEquiv(rate float64) float64 {
	return w.costUSD + w.costCNY/rate
}

type keyAdmission struct {
	mu     sync.Mutex
	st     *store.Store
	alerts *alertManager
	rpm    map[int64][]time.Time
	tpm    map[int64][]tokPoint
	day    map[int64]*windowUse
	month  map[int64]*windowUse
	cur    map[int64]int64 // 密钥级并发在请求数（ConcurrencyLimit>0 时维护）

	rateMu     sync.Mutex
	rateVal    float64
	rateExpire time.Time
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

// load 启动时从 key_usage_day 账本回填各密钥本日/本月用量。
func (a *keyAdmission) load(st *store.Store) {
	a.st = st
	keys, err := st.ListAPIKeys()
	if err != nil {
		return
	}
	dayStart := dayLabel()
	monthStart := monthLabel() + "-01"
	for i := range keys {
		k := &keys[i]
		if cu, cc, t, err := st.KeyUsageSince(k.ID, dayStart); err == nil {
			a.day[k.ID] = &windowUse{label: dayStart, costUSD: cu, costCNY: cc, tokens: t}
		}
		if cu, cc, t, err := st.KeyUsageSince(k.ID, monthStart); err == nil {
			a.month[k.ID] = &windowUse{label: monthLabel(), costUSD: cu, costCNY: cc, tokens: t}
		}
	}
}

// usdCnyRate 返回预算归一汇率（settings 表 usd_cny_rate，60s 缓存，默认 7.2）。
func (a *keyAdmission) usdCnyRate() float64 {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	if a.rateVal > 0 && time.Now().Before(a.rateExpire) {
		return a.rateVal
	}
	if a.st != nil {
		if v, err := a.st.GetSetting("usd_cny_rate"); err == nil {
			if f, perr := strconv.ParseFloat(v, 64); perr == nil && f > 0 {
				a.rateVal = f
			}
		}
	}
	if a.rateVal <= 0 {
		a.rateVal = defaultUSDCNYRate
	}
	a.rateExpire = time.Now().Add(time.Minute)
	return a.rateVal
}

// invalidateRate 预算汇率变更后立即生效（管理面 PUT 预算配置时调用）。
func (a *keyAdmission) invalidateRate() {
	a.rateMu.Lock()
	a.rateExpire = time.Time{}
	a.rateMu.Unlock()
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
			used := use.usdEquiv(a.usdCnyRate())
			if a.alerts != nil {
				a.alerts.fireBudget(ak.ID, ak.Name, use.label, ak.BudgetUSD, used, used >= ak.BudgetUSD)
			}
			if used >= ak.BudgetUSD {
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

// record 请求完成后累计 TPM 与预算用量；成本按价格表币种分列入账。
// 未启用任何限额时零开销直接返回——key_usage_day 账本由调用方（logRequest）
// 另行落账，与限额开关无关。
func (a *keyAdmission) record(ak *store.APIKey, prompt, completion int64, costUSD, costCNY float64) {
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
		w.costUSD += costUSD
		w.costCNY += costCNY
		w.tokens += prompt + completion
	}
}

// budgetUsage 返回密钥当前预算窗口的用量，供管理台画预算进度条：
// usd 为按汇率归一后的美元等值（与拦截判定同口径），cny 为人民币原始累计，
// tokens 为 token 数。未配预算返回 0。
func (a *keyAdmission) budgetUsage(ak *store.APIKey) (usd, cny float64, tokens int64) {
	if ak.BudgetUSD == 0 && ak.BudgetTokens == 0 {
		return 0, 0, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.budgetWindow(ak)
	return w.usdEquiv(a.usdCnyRate()), w.costCNY, w.tokens
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
	return strconv.FormatFloat(v, 'g', -1, 64)
}
