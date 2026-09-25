package store

import (
	"math"
	"sort"
	"strconv"
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

func (s *Store) Dashboard() (*Dashboard, error) {
	d := &Dashboard{
		Daily:     []UsagePoint{},
		ByModel:   []ModelUsage{},
		ByChannel: []ChannelUsage{},
		ByApp:     []AppUsage{},
	}
	today := `ts >= datetime('now', 'start of day')`
	err := s.DB.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM request_logs WHERE `+today+`),
			(SELECT COUNT(*) FROM request_logs WHERE `+today+` AND (status >= 400 OR error != '')),
			(SELECT IFNULL(SUM(prompt_tokens), 0) FROM request_logs WHERE `+today+`),
			(SELECT IFNULL(SUM(completion_tokens), 0) FROM request_logs WHERE `+today+`),
			(SELECT IFNULL(ROUND(SUM(cost), 6), 0) FROM request_logs WHERE `+today+`),
			(SELECT COUNT(*) FROM channels),
			(SELECT COUNT(*) FROM channels WHERE enabled = 1),
			(SELECT COUNT(*) FROM api_keys)`).Scan(
		&d.TodayRequests, &d.TodayErrors, &d.TodayPromptTokens,
		&d.TodayCompletionTokens, &d.TodayCostUSD, &d.Channels, &d.ChannelsEnabled, &d.Keys)
	if err != nil {
		return nil, err
	}

	rows, err := s.DB.Query(`
		SELECT date(ts), COUNT(*), IFNULL(SUM(status >= 400), 0),
		       IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_logs WHERE ` + sqlLast7Days + `
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

	rows, err = s.DB.Query(`
		SELECT model, COUNT(*), IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0),
		       IFNULL(SUM(cache_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0),
		       CASE WHEN IFNULL(SUM(latency_ms), 0) > 0
		            THEN ROUND(SUM(completion_tokens) * 1000.0 / SUM(latency_ms), 1) ELSE 0 END
		FROM request_logs WHERE ` + sqlLast7Days + ` AND model != ''
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
	rows, err = s.DB.Query(`
		SELECT r.channel_id, IFNULL(c.name, ''), COUNT(*),
		       IFNULL(SUM(r.prompt_tokens), 0), IFNULL(SUM(r.completion_tokens), 0),
		       IFNULL(SUM(r.cache_tokens), 0), IFNULL(ROUND(SUM(r.cost), 6), 0),
		       CASE WHEN IFNULL(SUM(r.latency_ms), 0) > 0
		            THEN ROUND(SUM(r.completion_tokens) * 1000.0 / SUM(r.latency_ms), 1) ELSE 0 END
		FROM request_logs r LEFT JOIN channels c ON c.id = r.channel_id
		WHERE r.` + sqlLast7Days + ` AND c.id IS NOT NULL
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

	// 今日成功请求的延迟明细：算 P50/P95 与平均生成速度（家庭量级直接全量取回）
	rows2, err := s.DB.Query(`
		SELECT latency_ms, ttfb_ms, completion_tokens FROM request_logs
		WHERE ` + today + ` AND status < 400 AND error = ''`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	var latencies []int64
	var genMs, genTok int64
	for rows2.Next() {
		var latency, ttfb, completion int64
		if err := rows2.Scan(&latency, &ttfb, &completion); err != nil {
			return nil, err
		}
		latencies = append(latencies, latency)
		if completion > 0 && latency > ttfb {
			genMs += latency - ttfb
			genTok += completion
		}
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	d.LatencyP50Ms = percentile(latencies, 0.50)
	d.LatencyP95Ms = percentile(latencies, 0.95)
	if genMs > 0 {
		d.AvgTps = float64(genTok) / (float64(genMs) / 1000)
	}

	// 实时 RPM/TPM（最近 60 秒）
	if err := s.DB.QueryRow(`
		SELECT COUNT(*), IFNULL(SUM(prompt_tokens + completion_tokens), 0)
		FROM request_logs WHERE ts >= datetime('now', '-60 seconds')`).
		Scan(&d.RPM, &d.TPM); err != nil {
		return nil, err
	}

	// 今日按应用分摊（X-LiteGate-App；空串显示为 "(未标注)"）
	rows3, err := s.DB.Query(`
		SELECT app, COUNT(*), IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0),
		       IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_logs WHERE ` + today + `
		GROUP BY app ORDER BY SUM(cost) DESC, COUNT(*) DESC`)
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
	prices, err := s.ListModelPrices()
	if err != nil {
		prices = nil
	}
	for i := range d.ByModel {
		if c := matchCurrency(prices, d.ByModel[i].Model); d.ByModel[i].CostUSD != 0 && c != "" {
			d.ByModel[i].CostByCurrency = map[string]float64{c: d.ByModel[i].CostUSD}
		}
	}

	todayBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(prices, `WHERE ts >= datetime('now', 'start of day')`, "", todayBuckets); err != nil {
		return nil, err
	}
	d.TodayCostByCurrency = todayBuckets[""]

	dayBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(prices, sqlLast7DaysWhere(), "date(ts)", dayBuckets); err != nil {
		return nil, err
	}
	for i := range d.Daily {
		d.Daily[i].CostByCurrency = dayBuckets[d.Daily[i].Day]
	}

	chBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(prices, sqlLast7DaysWhere(), "channel_id", chBuckets); err != nil {
		return nil, err
	}
	for i := range d.ByChannel {
		d.ByChannel[i].CostByCurrency = chBuckets[strconv.FormatInt(d.ByChannel[i].ChannelID, 10)]
	}

	appBuckets := map[string]map[string]float64{}
	if err := s.bucketModelCosts(prices, `WHERE ts >= datetime('now', 'start of day')`, "app", appBuckets); err != nil {
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

// sqlLast7DaysWhere 返回近 7 天（含今天，UTC）的 WHERE 片段（含 WHERE 关键字），
// 与 sqlLast7Days 常量口径一致。
func sqlLast7DaysWhere() string { return `WHERE ts >= datetime('now', '-6 days', 'start of day')` }

// matchCurrency 返回模型价格行的币种标注；无价格行时返回空串（成本本就记 0）。
func matchCurrency(prices []ModelPrice, model string) string {
	if p := MatchPrice(prices, model); p != nil {
		return p.Currency
	}
	return ""
}

// bucketModelCosts 把窗口内成本按「维度 × 模型」聚到币种桶：dim 为空时全部进 ""
// 单桶，否则按维度表达式分组（键以文本形式回传，如 channel_id 的十进制字符串）。
func (s *Store) bucketModelCosts(prices []ModelPrice, where, dim string, out map[string]map[string]float64) error {
	group := ""
	if dim != "" {
		group = dim + ", "
	}
	rows, err := s.DB.Query(`SELECT ` + group + `model, IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_logs ` + where + ` GROUP BY ` + group + `model`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, model string
		var cost float64
		if dim == "" {
			if err := rows.Scan(&model, &cost); err != nil {
				return err
			}
		} else {
			var dimKey any
			if err := rows.Scan(&dimKey, &model, &cost); err != nil {
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
		c := matchCurrency(prices, model)
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

// percentile 返回样本的最近秩分位数（向上取整索引）；样本就地排序，空样本返回 0。
func percentile(samples []int64, p float64) int64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := int(math.Ceil(p * float64(n)))
	if idx < 1 {
		idx = 1
	}
	if idx > n {
		idx = n
	}
	return samples[idx-1]
}

// TrendPoint 是趋势折线图的单（时间桶，对象）聚合点。
type TrendPoint struct {
	T                string  `json:"t"`      // 桶时间:小时桶 "YYYY-MM-DD HH:00" / 天桶 "YYYY-MM-DD"（本地时区）
	Tokens           int64   `json:"tokens"` // 输入+输出合计
	CacheTokens      int64   `json:"cache"`
	Cost             float64 `json:"cost"`
	Tps              float64 `json:"tps"` // 该桶平均输出速度（tok/s）
	PromptTokens     int64   `json:"prompt"`
	CompletionTokens int64   `json:"completion"`

	latencyMs int64  // 合并「其他」时重算 tps 用，不序列化
	name      string // 该行对象名（模型/渠道/密钥），合并时定位用，不序列化
	model     string // 该行原始模型名（币种判定用），不序列化
}

// TrendSeries 是趋势图里一个对象（模型/渠道/密钥）的完整序列。
type TrendSeries struct {
	Name   string       `json:"name"`
	Points []TrendPoint `json:"points"`
	// Currency 该序列成本的标注币种（序列内各模型价格币种中成本占比最高者），
	// 无成本时为空。
	Currency string `json:"currency,omitempty"`

	curTally map[string]float64 `json:"-"` // 币种 → 成本合计（组装期临时）
}

// UsageTrend 按 dim（model|channel|key）与 rng（today|yesterday|7d|30d）返回
// 折线图序列。今天/昨天按小时分桶、7/30 天按天分桶，均为本地时区；
// 对象按桶内 Token 总量取 Top8，其余合并为「其他」（tps 按总输出/总耗时重算）。
func (s *Store) UsageTrend(dim, rng string) (string, []TrendSeries, error) {
	var bucket, from string
	switch rng {
	case "today":
		bucket, from = `strftime('%Y-%m-%d %H:00', r.ts, 'localtime')`, `datetime('now','localtime','start of day','utc')`
	case "yesterday":
		bucket, from = `strftime('%Y-%m-%d %H:00', r.ts, 'localtime')`, `datetime('now','localtime','start of day','-1 day','utc')`
	case "30d":
		bucket, from = `date(r.ts, 'localtime')`, `datetime('now','localtime','start of day','-29 day','utc')`
	default: // 7d:含今天在内最近 7 个本地日
		bucket, from = `date(r.ts, 'localtime')`, `datetime('now','localtime','start of day','-6 day','utc')`
	}
	to := ""
	if rng == "yesterday" {
		to = ` AND r.ts < datetime('now','localtime','start of day','utc')`
	}
	join, name, dimWhere := ``, `r.model`, `r.model != ''`
	switch dim {
	case "channel":
		join, name, dimWhere = ` LEFT JOIN channels c ON c.id = r.channel_id`, `IFNULL(c.name, '')`, `c.id IS NOT NULL`
	case "key":
		join, name, dimWhere = ` LEFT JOIN api_keys k ON k.id = r.api_key_id`, `IFNULL(k.name, '')`, `k.id IS NOT NULL`
	}
	// 价格表先于主查询加载:连接池上限为 1,主查询迭代期间再开查询会自死锁
	prices, err := s.ListModelPrices()
	if err != nil {
		return "", nil, err
	}
	rows, err := s.DB.Query(`
		SELECT ` + bucket + `, ` + name + `, r.model, IFNULL(SUM(r.prompt_tokens), 0), IFNULL(SUM(r.completion_tokens), 0),
		       IFNULL(SUM(r.cache_tokens), 0), IFNULL(ROUND(SUM(r.cost), 6), 0), IFNULL(SUM(r.latency_ms), 0)
		FROM request_logs r` + join + `
		WHERE ` + dimWhere + ` AND r.ts >= ` + from + to + `
		GROUP BY 1, 2, 3 ORDER BY 1`)
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
		var b, n, m string
		if err := rows.Scan(&b, &n, &m, &p.PromptTokens, &p.CompletionTokens,
			&p.CacheTokens, &p.Cost, &p.latencyMs); err != nil {
			return "", nil, err
		}
		p.T = b
		p.name = n
		p.model = m
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
	sort.Slice(top, func(i, j int) bool { return totals[top[i]] > totals[top[j]] })
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
			ser = &TrendSeries{Name: n, curTally: map[string]float64{}}
			byName[n] = ser
			nameOrder = append(nameOrder, n)
		}
		if p.Cost != 0 {
			ser.curTally[matchCurrency(prices, p.model)] += p.Cost
		}
		// flat 已按桶排序;「其他」同一桶的多行在此累加成一个点
		if len(ser.Points) > 0 && ser.Points[len(ser.Points)-1].T == p.T {
			last := &ser.Points[len(ser.Points)-1]
			last.PromptTokens += p.PromptTokens
			last.CompletionTokens += p.CompletionTokens
			last.CacheTokens += p.CacheTokens
			last.Tokens += p.Tokens
			last.Cost += p.Cost
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
		best, bestSum := "", 0.0
		for c, sum := range ser.curTally {
			if math.Abs(sum) > math.Abs(bestSum) || (math.Abs(sum) == math.Abs(bestSum) && c < best) {
				best, bestSum = c, sum
			}
		}
		ser.Currency = best
		series = append(series, *ser)
	}
	return bucketKind, series, nil
}
