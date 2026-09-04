package store

// Dashboard 是仪表盘首屏的聚合统计。
type Dashboard struct {
	TodayRequests   int64 `json:"today_requests"`
	TodayErrors     int64 `json:"today_errors"`
	Channels        int64 `json:"channels"`
	ChannelsEnabled int64 `json:"channels_enabled"`
	Keys            int64 `json:"keys"`
}

func (s *Store) Dashboard() (*Dashboard, error) {
	d := &Dashboard{}
	row := s.DB.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM request_logs WHERE ts >= datetime('now', 'start of day')),
			(SELECT COUNT(*) FROM request_logs WHERE ts >= datetime('now', 'start of day') AND status >= 400),
			(SELECT COUNT(*) FROM channels),
			(SELECT COUNT(*) FROM channels WHERE enabled = 1),
			(SELECT COUNT(*) FROM api_keys)`)
	return d, row.Scan(&d.TodayRequests, &d.TodayErrors, &d.Channels, &d.ChannelsEnabled, &d.Keys)
}
