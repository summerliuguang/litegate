package store

import "strings"

type RequestLog struct {
	ID               int64   `json:"id"`
	Ts               string  `json:"ts"`
	APIKeyID         int64   `json:"api_key_id"`
	ChannelID        int64   `json:"channel_id"`
	Model            string  `json:"model"`
	Protocol         string  `json:"protocol"`
	Status           int     `json:"status"`
	LatencyMs        int64   `json:"latency_ms"`
	TtfbMs           int64   `json:"ttfb_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Error            string  `json:"error"`
}

func (s *Store) InsertRequestLog(l *RequestLog) error {
	res, err := s.DB.Exec(
		`INSERT INTO request_logs(api_key_id, channel_id, model, protocol, status,
		     latency_ms, ttfb_ms, prompt_tokens, completion_tokens, cost, error)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.APIKeyID, l.ChannelID, l.Model, l.Protocol, l.Status,
		l.LatencyMs, l.TtfbMs, l.PromptTokens, l.CompletionTokens, l.CostUSD, l.Error,
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
	ChannelID     int64
	APIKeyID      int64
	Model         string
	Status        string // ""=全部，"ok"=成功，"error"=失败
	Since, Until  string // "YYYY-MM-DD HH:MM:SS"，与 ts（UTC 文本）做字典序比较
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
		`SELECT id, ts, api_key_id, channel_id, model, protocol, status,
		        latency_ms, ttfb_ms, prompt_tokens, completion_tokens, cost, error
		 FROM request_logs` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.Ts, &l.APIKeyID, &l.ChannelID, &l.Model,
			&l.Protocol, &l.Status, &l.LatencyMs, &l.TtfbMs,
			&l.PromptTokens, &l.CompletionTokens, &l.CostUSD, &l.Error); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, l)
	}
	return page, rows.Err()
}

func (f LogFilter) where() (string, []any) {
	var conds []string
	var args []any
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
