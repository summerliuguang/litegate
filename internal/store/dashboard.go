package store

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// UsagePoint 是单日用量，Day 格式为 YYYY-MM-DD（UTC）。
type UsagePoint struct {
	Day              string  `json:"day"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// CostByCurrency 是成本按币种（价格表标注）的拆分；跨模型聚合里不同币种
	// 数值不可互加，前端按 "¥x + $y" 展示。
	CostByCurrency map[string]float64 `json:"cost_by_currency,omitempty"`
}

// ModelUsage / ChannelUsage 是近 7 天按维度聚合的用量，按费用降序。
type ModelUsage struct {
	Model            string             `json:"model"`
	Requests         int64              `json:"requests"`
	PromptTokens     int64              `json:"prompt_tokens"`
	CompletionTokens int64              `json:"completion_tokens"`
	CacheTokens      int64              `json:"cache_tokens"` // 缓存命中输入 token（prompt_tokens 的一部分）
	CostUSD          float64            `json:"cost_usd"`
	Tps              float64            `json:"tps"` // 近 7 天平均输出速度（tok/s，按总输出/总耗时）
	CostByCurrency   map[string]float64 `json:"cost_by_currency,omitempty"`
}

type ChannelUsage struct {
	ChannelID        int64              `json:"channel_id"`
	ChannelName      string             `json:"channel_name"`
	Requests         int64              `json:"requests"`
	PromptTokens     int64              `json:"prompt_tokens"`
	CompletionTokens int64              `json:"completion_tokens"`
	CacheTokens      int64              `json:"cache_tokens"`
	CostUSD          float64            `json:"cost_usd"`
	Tps              float64            `json:"tps"`
	CostByCurrency   map[string]float64 `json:"cost_by_currency,omitempty"`
}

// AppUsage 是今日按应用（X-LiteGate-App 请求头）分摊的用量，按费用降序。
type AppUsage struct {
	App              string             `json:"app"`
	Requests         int64              `json:"requests"`
	PromptTokens     int64              `json:"prompt_tokens"`
	CompletionTokens int64              `json:"completion_tokens"`
	CostUSD          float64            `json:"cost_usd"`
	CostByCurrency   map[string]float64 `json:"cost_by_currency,omitempty"`
}

// Dashboard 是仪表盘首屏的聚合统计。
type Dashboard struct {
	TodayRequests         int64   `json:"today_requests"`
	TodayErrors           int64   `json:"today_errors"`
	TodayPromptTokens     int64   `json:"today_prompt_tokens"`
	TodayCompletionTokens int64   `json:"today_completion_tokens"`
	TodayCostUSD          float64 `json:"today_cost_usd"`
	// TodayCostByCurrency 今日成本按币种拆分，语义同各聚合行的 CostByCurrency。
	TodayCostByCurrency map[string]float64 `json:"today_cost_by_currency,omitempty"`
	LatencyP50Ms        int64              `json:"latency_p50_ms"`
	LatencyP95Ms        int64              `json:"latency_p95_ms"`
	AvgTps              float64            `json:"avg_tps"` // 今日平均生成速度（输出 token/秒，成功请求）
	RPM                 int64              `json:"rpm"`     // 最近 60 秒请求数
	TPM                 int64              `json:"tpm"`     // 最近 60 秒 token 数
	Channels            int64              `json:"channels"`
	ChannelsEnabled     int64              `json:"channels_enabled"`
	Keys                int64              `json:"keys"`
	Daily               []UsagePoint       `json:"daily"`
	ByModel             []ModelUsage       `json:"by_model"`
	ByChannel           []ChannelUsage     `json:"by_channel"`
	ByApp               []AppUsage         `json:"by_app"`
}

// 近 7 天（含今天）的时间窗条件，ts 为 UTC 文本。
const sqlLast7Days = `ts >= datetime('now', '-6 days', 'start of day')`

func (s *Store) dashboard(db queryer, now time.Time) (*Dashboard, error) {
	d := &Dashboard{
		Daily:     []UsagePoint{},
		ByModel:   []ModelUsage{},
		ByChannel: []ChannelUsage{},
		ByApp:     []AppUsage{},
	}
	dayStart := utcDayStart(now)
	until := dayStart.AddDate(0, 0, 1)
	today := analyticsRange("ts", dayStart, until)
	last7Days := analyticsRange("ts", dayStart.AddDate(0, 0, -6), until)
	last7Channels := analyticsRange("r.ts", dayStart.AddDate(0, 0, -6), until)
	err := db.QueryRow(`
		SELECT IFNULL(SUM(requests), 0), IFNULL(SUM(failed_requests), 0),
		       IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0),
		       IFNULL(ROUND(SUM(cost), 6), 0),
		       (SELECT COUNT(*) FROM channels),
		       (SELECT COUNT(*) FROM channels WHERE enabled = 1),
		       (SELECT COUNT(*) FROM api_keys)
		FROM request_usage_hour WHERE `+today).Scan(
		&d.TodayRequests, &d.TodayErrors, &d.TodayPromptTokens,
		&d.TodayCompletionTokens, &d.TodayCostUSD, &d.Channels, &d.ChannelsEnabled, &d.Keys)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT date(ts), IFNULL(SUM(requests), 0), IFNULL(SUM(errors), 0),
		       IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_usage_hour WHERE ` + last7Days + `
		GROUP BY date(ts) ORDER BY date(ts)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p UsagePoint
		if err := rows.Scan(&p.Day, &p.Requests, &p.Errors,
			&p.PromptTokens, &p.CompletionTokens, &p.CostUSD); err != nil {
			rows.Close()
			return nil, err
		}
		d.Daily = append(d.Daily, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`
		SELECT model, IFNULL(SUM(requests), 0), IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0),
		       IFNULL(SUM(cache_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0),
		       CASE WHEN IFNULL(SUM(latency_ms), 0) > 0
		            THEN ROUND(SUM(completion_tokens) * 1000.0 / SUM(latency_ms), 1) ELSE 0 END
		FROM request_usage_hour WHERE ` + last7Days + ` AND model != ''
		GROUP BY model ORDER BY SUM(cost) DESC, model LIMIT 10`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.PromptTokens, &m.CompletionTokens,
			&m.CacheTokens, &m.CostUSD, &m.Tps); err != nil {
			rows.Close()
			return nil, err
		}
		d.ByModel = append(d.ByModel, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 渠道可能已被删除：只统计仍存在的渠道（已删除渠道不在榜单展示）
	rows, err = db.Query(`
		SELECT r.channel_id, IFNULL(c.name, ''), IFNULL(SUM(requests), 0),
		       IFNULL(SUM(r.prompt_tokens), 0), IFNULL(SUM(r.completion_tokens), 0),
		       IFNULL(SUM(r.cache_tokens), 0), IFNULL(ROUND(SUM(r.cost), 6), 0),
		       CASE WHEN IFNULL(SUM(r.latency_ms), 0) > 0
		            THEN ROUND(SUM(r.completion_tokens) * 1000.0 / SUM(r.latency_ms), 1) ELSE 0 END
		FROM request_usage_hour r LEFT JOIN channels c ON c.id = r.channel_id
		WHERE ` + last7Channels + ` AND c.id IS NOT NULL
		GROUP BY r.channel_id ORDER BY SUM(r.cost) DESC, r.channel_id LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch ChannelUsage
		if err := rows.Scan(&ch.ChannelID, &ch.ChannelName, &ch.Requests,
			&ch.PromptTokens, &ch.CompletionTokens, &ch.CacheTokens, &ch.CostUSD, &ch.Tps); err != nil {
			return nil, err
		}
		d.ByChannel = append(d.ByChannel, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Exact percentiles from a latency histogram: repeated latencies cross the
	// database boundary once instead of retaining every request in Go memory.
	rows2, err := db.Query(`
		SELECT latency_ms, IFNULL(SUM(requests), 0),
		       IFNULL(SUM(gen_ms), 0),
		       IFNULL(SUM(gen_tokens), 0)
		FROM request_latency_day WHERE ` + today + `
		GROUP BY latency_ms ORDER BY latency_ms`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	type latencyBucket struct{ latency, count int64 }
	var histogram []latencyBucket
	var genMs, genTok, totalSamples int64
	for rows2.Next() {
		var latency, count, ms, tokens int64
		if err := rows2.Scan(&latency, &count, &ms, &tokens); err != nil {
			return nil, err
		}
		histogram = append(histogram, latencyBucket{latency, count})
		totalSamples += count
		genMs += ms
		genTok += tokens
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	var seen int64
	p50, p95 := (totalSamples+1)/2, int64(math.Ceil(float64(totalSamples)*0.95))
	for _, bucket := range histogram {
		previous := seen
		seen += bucket.count
		if previous < p50 && seen >= p50 {
			d.LatencyP50Ms = bucket.latency
		}
		if previous < p95 && seen >= p95 {
			d.LatencyP95Ms = bucket.latency
		}
	}
	if genMs > 0 {
		d.AvgTps = float64(genTok) / (float64(genMs) / 1000)
	}

	// 实时 RPM/TPM（最近 60 秒）
	if err := db.QueryRow(`
		SELECT COUNT(*), IFNULL(SUM(prompt_tokens + completion_tokens), 0)
		FROM request_logs WHERE ts >= ? AND ts <= ?`, now.UTC().Add(-time.Minute).Format("2006-01-02 15:04:05"), now.UTC().Format("2006-01-02 15:04:05")).
		Scan(&d.RPM, &d.TPM); err != nil {
		return nil, err
	}

	// 今日按应用分摊（X-LiteGate-App；空串显示为 "(未标注)"）
	rows3, err := db.Query(`
		SELECT app, IFNULL(SUM(requests), 0), IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0),
		       IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_usage_hour WHERE ` + today + `
		GROUP BY app ORDER BY SUM(cost) DESC, IFNULL(SUM(requests), 0) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows3.Close()
	for rows3.Next() {
		var a AppUsage
		if err := rows3.Scan(&a.App, &a.Requests, &a.PromptTokens, &a.CompletionTokens, &a.CostUSD); err != nil {
			return nil, err
		}
		if a.App == "" {
			a.App = "(未标注)"
		}
		d.ByApp = append(d.ByApp, a)
	}
	if err := rows3.Err(); err != nil {
		return nil, err
	}

	// 成本按币种拆分：价格行标注了币种（USD/CNY），跨模型聚合的数值不可互加。
	// byModel 行模型唯一可直接标注；其余维度按「维度 × 模型」的模型成本桶进对应币种。
	prices, err := listModelPrices(db)
	if err != nil {
		prices = nil
	}
	modelBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(db, prices, "WHERE "+last7Days, "model", modelBuckets); err != nil {
		return nil, err
	}
	for i := range d.ByModel {
		d.ByModel[i].CostByCurrency = modelBuckets[d.ByModel[i].Model]
	}

	todayBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(db, prices, "WHERE "+today, "", todayBuckets); err != nil {
		return nil, err
	}
	d.TodayCostByCurrency = todayBuckets[""]

	dayBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(db, prices, "WHERE "+last7Days, "date(ts)", dayBuckets); err != nil {
		return nil, err
	}
	for i := range d.Daily {
		d.Daily[i].CostByCurrency = dayBuckets[d.Daily[i].Day]
	}

	chBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(db, prices, "WHERE "+last7Days, "channel_id", chBuckets); err != nil {
		return nil, err
	}
	for i := range d.ByChannel {
		d.ByChannel[i].CostByCurrency = chBuckets[strconv.FormatInt(d.ByChannel[i].ChannelID, 10)]
	}

	appBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(db, prices, "WHERE "+today, "app", appBuckets); err != nil {
		return nil, err
	}
	for i := range d.ByApp {
		name := d.ByApp[i].App
		if name == "(未标注)" {
			name = ""
		}
		d.ByApp[i].CostByCurrency = appBuckets[name]
	}
	return d, nil
}

// matchCurrency 返回模型价格行的币种标注；无价格行时返回空串（成本本就记 0）。
func matchCurrency(prices []ModelPrice, model string) string {
	if p := MatchPrice(prices, model); p != nil {
		return p.Currency
	}
	return ""
}

// bucketModelCosts 把窗口内成本按「维度 × 模型」聚到币种桶：dim 为空时全部进 ""
// 单桶，否则按维度表达式分组（键以文本形式回传，如 channel_id 的十进制字符串）。
func (s *Store) bucketModelCosts(db queryer, prices []ModelPrice, where, dim string, out map[string]map[string]float64) error {
	group := ""
	if dim != "" {
		group = dim + ", "
	}
	rows, err := db.Query(`SELECT ` + group + `model, currency, IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_usage_hour ` + where + ` GROUP BY ` + group + `model, currency`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, model, currency string
		var cost float64
		if dim == "" {
			if err := rows.Scan(&model, &currency, &cost); err != nil {
				return err
			}
		} else {
			var dimKey any
			if err := rows.Scan(&dimKey, &model, &currency, &cost); err != nil {
				return err
			}
			switch k := dimKey.(type) {
			case string:
				key = k
			case []byte:
				key = string(k)
			case int64:
				key = strconv.FormatInt(k, 10)
			case float64:
				key = strconv.FormatInt(int64(k), 10)
			}
		}
		if cost == 0 {
			continue
		}
		c := currency
		if c == "" {
			c = matchCurrency(prices, model)
		}
		if c == "" {
			c = "USD"
		}
		if out[key] == nil {
			out[key] = map[string]float64{}
		}
		out[key][c] = math.Round((out[key][c]+cost)*1e6) / 1e6
	}
	return rows.Err()
}

// TrendPoint 是趋势折线图的单（时间桶，对象）聚合点。
type TrendPoint struct {
	T                string             `json:"t"`      // 桶时间:小时桶 "YYYY-MM-DD HH:00" / 天桶 "YYYY-MM-DD"（UTC）
	Tokens           int64              `json:"tokens"` // 输入+输出合计
	CacheTokens      int64              `json:"cache"`
	Cost             float64            `json:"cost"` // 单币种兼容字段；混合币种为0，使用 CostByCurrency
	CostByCurrency   map[string]float64 `json:"cost_by_currency"`
	Tps              float64            `json:"tps"` // 该桶平均输出速度（tok/s）
	PromptTokens     int64              `json:"prompt"`
	CompletionTokens int64              `json:"completion"`

	latencyMs int64  // 合并「其他」时重算 tps 用，不序列化
	name      string // 该行对象名（模型/渠道/密钥），合并时定位用，不序列化
}

// TrendSeries 是趋势图里一个对象（模型/渠道/密钥）的完整序列。
type TrendSeries struct {
	Name   string       `json:"name"`
	Points []TrendPoint `json:"points"`
	// Currency 单币种序列的兼容标注；混合币种或无成本时为空。
	Currency string `json:"currency,omitempty"`
}

// UsageTrend 按 dim（model|channel|key）与 rng（today|yesterday|7d|30d）返回
// 折线图序列。今天/昨天按小时分桶、7/30 天按天分桶，均为 UTC；
// 对象按桶内 Token 总量取 Top8，其余合并为「其他」（tps 按总输出/总耗时重算）。
func (s *Store) usageTrend(db queryer, now time.Time, dim, rng string) (string, []TrendSeries, error) {
	dayStart := utcDayStart(now)
	from, until := dayStart.AddDate(0, 0, -6), dayStart.AddDate(0, 0, 1)
	bucket := `date(r.ts)`
	switch rng {
	case "today":
		bucket = `strftime('%Y-%m-%d %H:00',r.ts)`
		from = dayStart
	case "yesterday":
		bucket = `strftime('%Y-%m-%d %H:00',r.ts)`
		from = dayStart.AddDate(0, 0, -1)
		until = dayStart
	case "30d":
		from = dayStart.AddDate(0, 0, -29)
	}
	window := analyticsRange("r.ts", from, until)
	join, name, dimWhere := ``, `r.model`, `r.model != ''`
	switch dim {
	case "channel":
		join, name, dimWhere = ` LEFT JOIN channels c ON c.id = r.channel_id`, `IFNULL(c.name, '')`, `c.id IS NOT NULL`
	case "key":
		join, name, dimWhere = ` LEFT JOIN api_keys k ON k.id = r.api_key_id`, `IFNULL(k.name, '')`, `k.id IS NOT NULL`
	}
	// 价格表先于主查询加载:统计连接池上限为 1,主查询迭代期间再开查询会自死锁
	prices, err := listModelPrices(db)
	if err != nil {
		return "", nil, err
	}
	rows, err := db.Query(`
		SELECT ` + bucket + `, ` + name + `, r.model, r.currency, IFNULL(SUM(r.prompt_tokens), 0), IFNULL(SUM(r.completion_tokens), 0),
		       IFNULL(SUM(r.cache_tokens), 0), IFNULL(ROUND(SUM(r.cost), 6), 0), IFNULL(SUM(r.latency_ms), 0)
		FROM request_usage_hour r` + join + `
		WHERE ` + dimWhere + ` AND ` + window + `
		GROUP BY 1, 2, 3, 4 ORDER BY 1`)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	bucketKind := "day"
	if rng == "today" || rng == "yesterday" {
		bucketKind = "hour"
	}
	totals := map[string]int64{}
	var flat []TrendPoint
	for rows.Next() {
		var p TrendPoint
		var b, n, m, currency string
		if err := rows.Scan(&b, &n, &m, &currency, &p.PromptTokens, &p.CompletionTokens,
			&p.CacheTokens, &p.Cost, &p.latencyMs); err != nil {
			return "", nil, err
		}
		p.T = b
		p.name = n
		if currency == "" {
			currency = matchCurrency(prices, m)
		}
		if currency == "" {
			currency = "USD"
		}
		p.CostByCurrency = map[string]float64{currency: p.Cost}
		p.Tokens = p.PromptTokens + p.CompletionTokens
		if p.latencyMs > 0 {
			p.Tps = math.Round(float64(p.CompletionTokens)*1000/float64(p.latencyMs)*10) / 10
		}
		totals[n] += p.Tokens
		flat = append(flat, p)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	top := make([]string, 0, 9)
	for n := range totals {
		top = append(top, n)
	}
	sort.Slice(top, func(i, j int) bool {
		if totals[top[i]] == totals[top[j]] {
			return top[i] < top[j]
		}
		return totals[top[i]] > totals[top[j]]
	})
	if len(top) > 8 {
		top = top[:8]
	}
	keep := map[string]bool{"其他": true}
	for _, n := range top {
		keep[n] = true
	}
	// Top8 之外的模型并入「其他」:同一（桶,对象）的多行先聚合，再按首次出现顺序成序列
	byName := map[string]*TrendSeries{}
	var nameOrder []string
	for _, p := range flat {
		n := p.name
		if !keep[n] {
			n = "其他"
		}
		ser := byName[n]
		if ser == nil {
			ser = &TrendSeries{Name: n}
			byName[n] = ser
			nameOrder = append(nameOrder, n)
		}

		// flat 已按桶排序;「其他」同一桶的多行在此累加成一个点
		if len(ser.Points) > 0 && ser.Points[len(ser.Points)-1].T == p.T {
			last := &ser.Points[len(ser.Points)-1]
			last.PromptTokens += p.PromptTokens
			last.CompletionTokens += p.CompletionTokens
			last.CacheTokens += p.CacheTokens
			last.Tokens += p.Tokens
			for currency, cost := range p.CostByCurrency {
				last.CostByCurrency[currency] += cost
			}
			last.latencyMs += p.latencyMs
			if last.latencyMs > 0 {
				last.Tps = math.Round(float64(last.CompletionTokens)*1000/float64(last.latencyMs)*10) / 10
			}
		} else {
			if p.latencyMs > 0 {
				p.Tps = math.Round(float64(p.CompletionTokens)*1000/float64(p.latencyMs)*10) / 10
			}
			ser.Points = append(ser.Points, p)
		}
	}
	series := make([]TrendSeries, 0, len(nameOrder))
	for _, n := range nameOrder {
		ser := byName[n]
		currencies := map[string]bool{}
		for i := range ser.Points {
			point := &ser.Points[i]
			point.Cost = 0
			for c, cost := range point.CostByCurrency {
				cost = math.Round(cost*1e6) / 1e6
				point.CostByCurrency[c] = cost
				currencies[c] = true
				if len(point.CostByCurrency) == 1 {
					point.Cost = cost
				}
			}
		}
		if len(currencies) == 1 {
			for c := range currencies {
				ser.Currency = c
			}
		}

		series = append(series, *ser)
	}
	return bucketKind, series, nil
}
