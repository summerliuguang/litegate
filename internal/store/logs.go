package store

type RequestLog struct {
	ID        int64  `json:"id"`
	Ts        string `json:"ts"`
	APIKeyID  int64  `json:"api_key_id"`
	ChannelID int64  `json:"channel_id"`
	Model     string `json:"model"`
	Protocol  string `json:"protocol"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error"`
}

func (s *Store) InsertRequestLog(l *RequestLog) error {
	res, err := s.DB.Exec(
		`INSERT INTO request_logs(api_key_id, channel_id, model, protocol, status, latency_ms, error)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		l.APIKeyID, l.ChannelID, l.Model, l.Protocol, l.Status, l.LatencyMs, l.Error,
	)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) ListRequestLogs(limit int) ([]RequestLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(
		`SELECT id, ts, api_key_id, channel_id, model, protocol, status, latency_ms, error
		 FROM request_logs ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestLog
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.Ts, &l.APIKeyID, &l.ChannelID, &l.Model,
			&l.Protocol, &l.Status, &l.LatencyMs, &l.Error); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
