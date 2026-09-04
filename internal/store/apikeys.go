package store

import (
	"database/sql"
	"errors"

	"litegate/internal/cryptoutil"
)

// APIKey 是下游客户端访问网关用的虚拟密钥。
type APIKey struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
}

// CreateAPIKey 生成并持久化一个新的 sk- 前缀虚拟密钥。
func (s *Store) CreateAPIKey(name string) (*APIKey, error) {
	k := &APIKey{Key: "sk-lg-" + cryptoutil.RandomHex(16), Name: name, Enabled: true}
	res, err := s.DB.Exec(`INSERT INTO api_keys(key, name, enabled) VALUES(?, ?, 1)`, k.Key, k.Name)
	if err != nil {
		return nil, err
	}
	if k.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return k, nil
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.DB.Query(`SELECT id, key, name, enabled, created_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &enabled, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		out = append(out, k)
	}
	return out, rows.Err()
}

// LookupAPIKey 仅匹配处于启用状态的密钥。
func (s *Store) LookupAPIKey(key string) (*APIKey, error) {
	var k APIKey
	var enabled int
	err := s.DB.QueryRow(
		`SELECT id, key, name, enabled, created_at FROM api_keys WHERE key = ? AND enabled = 1`, key,
	).Scan(&k.ID, &k.Key, &k.Name, &enabled, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *Store) DeleteAPIKey(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountAPIKeys() (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n, err
}
