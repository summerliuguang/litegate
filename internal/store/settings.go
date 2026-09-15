package store

// settings 表的通用读写：告警配置等运行时配置存这里（JSON 文本），管理面修改
// 即时生效，无需重启。

import (
	"database/sql"
	"errors"
)

// GetSetting 读取单个设置项；不存在返回 ErrNotFound。
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetSetting 写入（upsert）单个设置项。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AdminAudit 是一条管理操作审计记录（ts 为 UTC "YYYY-MM-DD HH:MM:SS"）。
type AdminAudit struct {
	ID     int64  `json:"id"`
	Ts     string `json:"ts"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	IP     string `json:"ip"`
}

// InsertAudit 记录一条管理操作；超过 1000 条时顺带清理最旧的（低频操作，开销可忽略）。
func (s *Store) InsertAudit(action, detail, ip string) error {
	_, err := s.DB.Exec(`INSERT INTO admin_audit(action, detail, ip) VALUES(?, ?, ?)`, action, detail, ip)
	if err != nil {
		return err
	}
	_, _ = s.DB.Exec(`DELETE FROM admin_audit WHERE id <= (SELECT MAX(id) - 1000 FROM admin_audit)`)
	return nil
}

// ListAudit 返回最近的审计记录（新→旧）。
func (s *Store) ListAudit(limit int) ([]AdminAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.DB.Query(
		`SELECT id, ts, action, detail, ip FROM admin_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAudit
	for rows.Next() {
		var a AdminAudit
		if err := rows.Scan(&a.ID, &a.Ts, &a.Action, &a.Detail, &a.IP); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
