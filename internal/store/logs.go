package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type RequestLog struct {
	ID               int64   `json:"id"`
	Ts               string  `json:"ts"`
	APIKeyID         int64   `json:"api_key_id"`
	ChannelID        int64   `json:"channel_id"`
	Model            string  `json:"model"`
	Protocol         string  `json:"protocol"`
	App              string  `json:"app"`
	Status           int     `json:"status"`
	LatencyMs        int64   `json:"latency_ms"`
	TtfbMs           int64   `json:"ttfb_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheTokens      int64   `json:"cache_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Error            string  `json:"error"`
	// Currency 由查询侧按价格表回填（该模型价格的币种），不落库。
	Currency string `json:"currency,omitempty"`
}

func (s *Store) InsertRequestLog(l *RequestLog) error {
	res, err := s.DB.Exec(
		`INSERT INTO request_logs(api_key_id, channel_id, model, protocol, app, status,
		     latency_ms, ttfb_ms, prompt_tokens, completion_tokens, cache_tokens, cost, error)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.APIKeyID, l.ChannelID, l.Model, l.Protocol, l.App, l.Status,
		l.LatencyMs, l.TtfbMs, l.PromptTokens, l.CompletionTokens, l.CacheTokens, l.CostUSD, l.Error,
	)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

// LogFilter 是请求日志的查询条件；零值表示不过滤。
type LogFilter struct {
	Limit, Offset int
	// ID 精确匹配单条日志（request_id 直达排查）；0 = 不启用。
	ID           int64
	ChannelID    int64
	APIKeyID     int64
	Model        string
	App          string
	Status       string // ""=全部，"ok"=成功，"error"=失败
	Since, Until string // "YYYY-MM-DD HH:MM:SS"，与 ts（UTC 文本）做字典序比较
}

// LogPage 的 total 是当前过滤条件下的总条数，供分页展示。
type LogPage struct {
	Total int64        `json:"total"`
	Items []RequestLog `json:"items"`
}

func (s *Store) ListLogs(f LogFilter) (*LogPage, error) {
	where, args := f.where()
	page := &LogPage{Items: []RequestLog{}}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&page.Total); err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	rows, err := s.DB.Query(
		`SELECT id, ts, api_key_id, channel_id, model, protocol, app, status,
		        latency_ms, ttfb_ms, prompt_tokens, completion_tokens, cache_tokens, cost, error
		 FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.Ts, &l.APIKeyID, &l.ChannelID, &l.Model,
			&l.Protocol, &l.App, &l.Status, &l.LatencyMs, &l.TtfbMs,
			&l.PromptTokens, &l.CompletionTokens, &l.CacheTokens, &l.CostUSD, &l.Error); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, l)
	}
	return page, rows.Err()
}

func (f LogFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ID > 0 {
		conds = append(conds, "id = ?")
		args = append(args, f.ID)
	}
	if f.ChannelID > 0 {
		conds = append(conds, "channel_id = ?")
		args = append(args, f.ChannelID)
	}
	if f.APIKeyID > 0 {
		conds = append(conds, "api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.App != "" {
		conds = append(conds, "app = ?")
		args = append(args, f.App)
	}
	switch f.Status {
	case "ok":
		conds = append(conds, "status < 400 AND error = ''")
	case "error":
		conds = append(conds, "(status >= 400 OR error != '')")
	}
	if f.Since != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until != "" {
		conds = append(conds, "ts <= ?")
		args = append(args, f.Until)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// LogSummary 是按模型聚合的请求摘要(日志钻取第一层:模型卡片)。
type LogSummary struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheTokens      int64   `json:"cache_tokens"`
	Cost             float64 `json:"cost"`
	Currency         string  `json:"currency,omitempty"`
	LastTs           string  `json:"last_ts"`
}

// LogSummaries 按现有过滤条件聚合每个模型的请求摘要(不含未记录模型名的失败请求)。
func (s *Store) LogSummaries(f LogFilter) ([]LogSummary, error) {
	where, args := f.where()
	if where == "" {
		where = " WHERE model != ''"
	} else {
		where += " AND model != ''"
	}
	rows, err := s.DB.Query(
		`SELECT model, COUNT(*), IFNULL(SUM(status >= 400 OR error != ''), 0),
		        IFNULL(SUM(prompt_tokens), 0), IFNULL(SUM(completion_tokens), 0), IFNULL(SUM(cache_tokens), 0),
		        IFNULL(ROUND(SUM(cost), 6), 0), MAX(ts)
		 FROM request_logs`+where+`
		 GROUP BY model ORDER BY MAX(ts) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogSummary{}
	for rows.Next() {
		var m LogSummary
		if err := rows.Scan(&m.Model, &m.Requests, &m.Errors, &m.PromptTokens,
			&m.CompletionTokens, &m.CacheTokens, &m.Cost, &m.LastTs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// KeyDailyUsageRow 是某密钥单天的用量聚合（趋势图用）。
type KeyDailyUsageRow struct {
	Day      string  `json:"day"` // UTC "2026-09-17"
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	Tokens   int64   `json:"tokens"` // 输入 + 输出
	Cost     float64 `json:"cost"`
}

// KeyDailyUsage 按天聚合某密钥近 days 天的用量（UTC 日对齐，与日志 ts 口径一致）。
func (s *Store) KeyDailyUsage(keyID int64, days int) ([]KeyDailyUsageRow, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02") + " 00:00:00"
	rows, err := s.DB.Query(
		`SELECT substr(ts,1,10) AS day, COUNT(*), IFNULL(SUM(status >= 400 OR error != ''), 0),
		        IFNULL(SUM(prompt_tokens + completion_tokens), 0), IFNULL(ROUND(SUM(cost), 6), 0)
		 FROM request_logs
		 WHERE api_key_id = ? AND ts >= ?
		 GROUP BY day ORDER BY day`, keyID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyDailyUsageRow{}
	for rows.Next() {
		var r KeyDailyUsageRow
		if err := rows.Scan(&r.Day, &r.Requests, &r.Errors, &r.Tokens, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneLogs 删除 days 天前的请求日志，返回删除行数；days <= 0 时不删除。
// 由调用方按保留策略周期性调用（SQLite 单连接，删除大表时段短暂占用写锁）。
func (s *Store) PruneLogs(days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	res, err := s.DB.Exec(`DELETE FROM request_logs WHERE ts < datetime('now', ?)`,
		fmt.Sprintf("-%d days", days))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequestBody 是一笔请求的采样正文（仅非流式；req/resp 截断到配置上限）。
type RequestBody struct {
	LogID    int64  `json:"log_id"`
	Ts       string `json:"ts"`
	Model    string `json:"model"`
	APIKeyID int64  `json:"api_key_id"`
	ReqBody  string `json:"req_body"`
	RespBody string `json:"resp_body"`
}

// InsertRequestBody 落一条采样正文。
func (s *Store) InsertRequestBody(logID int64, model string, apiKeyID int64, reqBody, respBody string) error {
	_, err := s.DB.Exec(
		`INSERT INTO request_bodies(log_id, model, api_key_id, req_body, resp_body) VALUES(?, ?, ?, ?, ?)`,
		logID, model, apiKeyID, reqBody, respBody)
	return err
}

// GetRequestBody 取回采样正文；未留存返回 ErrNotFound。
func (s *Store) GetRequestBody(logID int64) (*RequestBody, error) {
	var rb RequestBody
	err := s.DB.QueryRow(
		`SELECT log_id, ts, model, api_key_id, req_body, resp_body FROM request_bodies WHERE log_id = ?`, logID,
	).Scan(&rb.LogID, &rb.Ts, &rb.Model, &rb.APIKeyID, &rb.ReqBody, &rb.RespBody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rb, nil
}

// PruneRequestBodies 清理超过天数（按 UTC 天）的采样正文，返回删除行数。
func (s *Store) PruneRequestBodies(days int) (int64, error) {
	res, err := s.DB.Exec(
		`DELETE FROM request_bodies WHERE ts < datetime('now', ?)`, fmt.Sprintf("-%d days", days))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
