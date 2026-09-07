package store

import (
	"math"
	"sort"
)

// UsagePoint 是单日用量，Day 格式为 YYYY-MM-DD（UTC）。
type UsagePoint struct {
	Day              string  `json:"day"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// ModelUsage / ChannelUsage 是近 7 天按维度聚合的用量，按费用降序。
type ModelUsage struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type ChannelUsage struct {
	ChannelID        int64   `json:"channel_id"`
	ChannelName      string  `json:"channel_name"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// AppUsage 是今日按应用（X-LiteGate-App 请求头）分摊的用量，按费用降序。
type AppUsage struct {
	App              string  `json:"app"`
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Dashboard 是仪表盘首屏的聚合统计。
type Dashboard struct {
	TodayRequests         int64          `json:"today_requests"`
	TodayErrors           int64          `json:"today_errors"`
	TodayPromptTokens     int64          `json:"today_prompt_tokens"`
	TodayCompletionTokens int64          `json:"today_completion_tokens"`
	TodayCostUSD          float64        `json:"today_cost_usd"`
	LatencyP50Ms          int64          `json:"latency_p50_ms"`
	LatencyP95Ms          int64          `json:"latency_p95_ms"`
	AvgTps                float64        `json:"avg_tps"` // 今日平均生成速度（输出 token/秒，成功请求）
	RPM                   int64          `json:"rpm"`     // 最近 60 秒请求数
	TPM                   int64          `json:"tpm"`     // 最近 60 秒 token 数
	Channels              int64          `json:"channels"`
	ChannelsEnabled       int64          `json:"channels_enabled"`
	Keys                  int64          `json:"keys"`
	Daily                 []UsagePoint   `json:"daily"`
	ByModel               []ModelUsage   `json:"by_model"`
	ByChannel             []ChannelUsage `json:"by_channel"`
	ByApp                 []AppUsage     `json:"by_app"`
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
			(SELECT COUNT(*) FROM request_logs WHERE ` + today + `),
			(SELECT COUNT(*) FROM request_logs WHERE ` + today + ` AND (status >= 400 OR error != '')),
			(SELECT IFNULL(SUM(prompt_tokens), 0) FROM request_logs WHERE ` + today + `),
			(SELECT IFNULL(SUM(completion_tokens), 0) FROM request_logs WHERE ` + today + `),
			(SELECT IFNULL(ROUND(SUM(cost), 6), 0) FROM request_logs WHERE ` + today + `),
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
		SELECT model, COUNT(*), IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0)
		FROM request_logs WHERE ` + sqlLast7Days + `
		GROUP BY model ORDER BY SUM(cost) DESC, model LIMIT 10`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Requests, &m.PromptTokens, &m.CompletionTokens, &m.CostUSD); err != nil {
			rows.Close()
			return nil, err
		}
		d.ByModel = append(d.ByModel, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 渠道可能已被删除，左连接取名字，名字留空
	rows, err = s.DB.Query(`
		SELECT r.channel_id, IFNULL(c.name, ''), COUNT(*),
		       IFNULL(SUM(r.prompt_tokens), 0), IFNULL(SUM(r.completion_tokens), 0), IFNULL(ROUND(SUM(r.cost), 6), 0)
		FROM request_logs r LEFT JOIN channels c ON c.id = r.channel_id
		WHERE r.` + sqlLast7Days + `
		GROUP BY r.channel_id ORDER BY SUM(r.cost) DESC, r.channel_id LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch ChannelUsage
		if err := rows.Scan(&ch.ChannelID, &ch.ChannelName, &ch.Requests,
			&ch.PromptTokens, &ch.CompletionTokens, &ch.CostUSD); err != nil {
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
	return d, rows3.Err()
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
