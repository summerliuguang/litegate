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
