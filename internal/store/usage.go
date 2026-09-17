package store

// 密钥按天用量累计（key_usage_day）：预算回填与用量趋势的统一账本。
// request_logs 按保留期清理（生产 7 天），月预算的回填不能依赖它——
// 本表独立生存、按 (api_key_id, day) 主键 UPSERT 累加，成本按币种拆列。

import (
	"fmt"
	"time"
)

// AddKeyUsageDay 把一笔请求计入密钥的当日用量（at 取 UTC 日）。
// 每笔请求一次轻量 UPSERT，单连接串行下与日志 INSERT 同量级。
func (s *Store) AddKeyUsageDay(apiKeyID int64, at time.Time, requests, errs, tokens int64, costUSD, costCNY float64) error {
	day := at.UTC().Format("2006-01-02")
	_, err := s.DB.Exec(
		`INSERT INTO key_usage_day(api_key_id, day, requests, errors, tokens, cost_usd, cost_cny)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(api_key_id, day) DO UPDATE SET
		   requests  = requests  + excluded.requests,
		   errors    = errors    + excluded.errors,
		   tokens    = tokens    + excluded.tokens,
		   cost_usd  = cost_usd  + excluded.cost_usd,
		   cost_cny  = cost_cny  + excluded.cost_cny`,
		apiKeyID, day, requests, errs, tokens, costUSD, costCNY)
	return err
}

// KeyUsageSince 汇总密钥自某 UTC 日（含）以来的用量，供预算窗口回填。
// sinceDay 形如 "2026-09-17"；月窗口传当月 1 号。
func (s *Store) KeyUsageSince(apiKeyID int64, sinceDay string) (costUSD, costCNY float64, tokens int64, err error) {
	err = s.DB.QueryRow(
		`SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(cost_cny),0), COALESCE(SUM(tokens),0)
		 FROM key_usage_day WHERE api_key_id = ? AND day >= ?`, apiKeyID, sinceDay,
	).Scan(&costUSD, &costCNY, &tokens)
	return costUSD, costCNY, tokens, err
}

// PruneKeyUsageDays 删除 days 天前的按日用量；预算最长窗口是月，默认保留
// 400 天余量充足。days <= 0 时不删除。返回删除行数。
func (s *Store) PruneKeyUsageDays(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	res, err := s.DB.Exec(`DELETE FROM key_usage_day WHERE day < ?`,
		time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// KeyDailyUsageRow 是某密钥单天的用量聚合（趋势图/预算回填共用）。
type KeyDailyUsageRow struct {
	Day      string  `json:"day"` // UTC "2026-09-17"
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	Tokens   int64   `json:"tokens"` // 输入 + 输出
	CostUSD  float64 `json:"cost_usd"`
	CostCNY  float64 `json:"cost_cny"`
}

// KeyDailyUsage 按天聚合某密钥近 days 天的用量（UTC 日对齐），读 key_usage_day
// 账本——日志清理不影响趋势窗口。
func (s *Store) KeyDailyUsage(keyID int64, days int) ([]KeyDailyUsageRow, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	rows, err := s.DB.Query(
		`SELECT day, requests, errors, tokens, cost_usd, cost_cny
		 FROM key_usage_day
		 WHERE api_key_id = ? AND day >= ?
		 ORDER BY day`, keyID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyDailyUsageRow{}
	for rows.Next() {
		var r KeyDailyUsageRow
		if err := rows.Scan(&r.Day, &r.Requests, &r.Errors, &r.Tokens, &r.CostUSD, &r.CostCNY); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BackfillKeyUsageFromLogs 把 request_logs 里的历史用量一次性聚合进
// key_usage_day（仅当表为空且日志非空时执行，天然幂等）。币种按当前价格表
// 逐模型判定（MatchPrice 前缀匹配无法用 SQL JOIN 表达，在 Go 侧聚合）；
// 改过币种的历史行按现值近似，可接受。返回迁移的 (密钥, 日) 组数。
func (s *Store) BackfillKeyUsageFromLogs() (int, error) {
	var haveLogs, haveUsage int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&haveLogs); err != nil {
		return 0, err
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM key_usage_day`).Scan(&haveUsage); err != nil {
		return 0, err
	}
	if haveLogs == 0 || haveUsage > 0 {
		return 0, nil
	}
	prices, err := s.ListModelPrices()
	if err != nil {
		return 0, err
	}
	rows, err := s.DB.Query(
		`SELECT api_key_id, substr(ts,1,10), model, COUNT(*),
		        IFNULL(SUM(status >= 400 OR error != ''), 0),
		        IFNULL(SUM(prompt_tokens + completion_tokens), 0), IFNULL(SUM(cost), 0)
		 FROM request_logs
		 WHERE api_key_id > 0
		 GROUP BY api_key_id, substr(ts,1,10), model`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type key struct {
		keyID int64
		day   string
	}
	agg := map[key]*KeyDailyUsageRow{}
	for rows.Next() {
		var keyID int64
		var day, model string
		var reqs, errs, tokens int64
		var cost float64
		if err := rows.Scan(&keyID, &day, &model, &reqs, &errs, &tokens, &cost); err != nil {
			return 0, err
		}
		a := agg[key{keyID, day}]
		if a == nil {
			a = &KeyDailyUsageRow{Day: day}
			agg[key{keyID, day}] = a
		}
		a.Requests += reqs
		a.Errors += errs
		a.Tokens += tokens
		if p := MatchPrice(prices, model); p != nil && p.Currency == "CNY" {
			a.CostCNY += cost
		} else {
			a.CostUSD += cost
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for keyIDDay, a := range agg {
		// BackfillKeyUsageFromLogs 只在空表时进入此处，直接 INSERT
		if _, err := s.DB.Exec(
			`INSERT INTO key_usage_day(api_key_id, day, requests, errors, tokens, cost_usd, cost_cny)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			keyIDDay.keyID, a.Day, a.Requests, a.Errors, a.Tokens, a.CostUSD, a.CostCNY); err != nil {
			return n, fmt.Errorf("backfill %d/%s: %w", keyIDDay.keyID, a.Day, err)
		}
		n++
	}
	return n, nil
}
