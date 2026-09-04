package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"litegate/internal/cryptoutil"
)

// Channel 是一个上游渠道：一种协议 + 一个入口 + 一组凭证与路由参数。
type Channel struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"` // openai | anthropic
	BaseURL   string   `json:"base_url"`
	APIKey    string   `json:"-"` // 解密后的明文，仅供代理转发使用，禁止序列化
	Models    []string `json:"models"` // 可服务的模型；为空表示全部
	Weight    int      `json:"weight"`
	Priority  int      `json:"priority"`
	Enabled   bool     `json:"enabled"`
	Remark    string   `json:"remark"`
	CreatedAt string   `json:"created_at"`
}

func (s *Store) CreateChannel(c *Channel) (int64, error) {
	if c.Weight <= 0 {
		c.Weight = 1
	}
	models, err := json.Marshal(c.Models)
	if err != nil {
		return 0, err
	}
	enc, err := cryptoutil.Encrypt(c.APIKey, s.secret)
	if err != nil {
		return 0, err
	}
	res, err := s.DB.Exec(
		`INSERT INTO channels(name, type, base_url, api_key, models, weight, priority, enabled, remark)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"), enc,
		string(models), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateChannel(c *Channel) error {
	if c.Weight <= 0 {
		c.Weight = 1
	}
	models, err := json.Marshal(c.Models)
	if err != nil {
		return err
	}
	enc, err := cryptoutil.Encrypt(c.APIKey, s.secret)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(
		`UPDATE channels SET name=?, type=?, base_url=?, api_key=?, models=?,
		 weight=?, priority=?, enabled=?, remark=? WHERE id=?`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"), enc,
		string(models), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark, c.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteChannel(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetChannel(id int64) (*Channel, error) {
	return s.scanChannel(s.DB.QueryRow(
		`SELECT id, name, type, base_url, api_key, models, weight, priority, enabled, remark, created_at
		 FROM channels WHERE id = ?`, id,
	))
}

// ListChannels 按 type 过滤（空串表示全部），优先级高的在前。
func (s *Store) ListChannels(typ string) ([]Channel, error) {
	q := `SELECT id, name, type, base_url, api_key, models, weight, priority, enabled, remark, created_at FROM channels`
	var args []any
	if typ != "" {
		q += ` WHERE type = ?`
		args = append(args, typ)
	}
	q += ` ORDER BY priority DESC, weight DESC, id`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := s.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func (s *Store) scanChannel(row scanner) (*Channel, error) {
	var c Channel
	var models, enc string
	var enabled int
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &enc, &models,
		&c.Weight, &c.Priority, &enabled, &c.Remark, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if models != "" {
		_ = json.Unmarshal([]byte(models), &c.Models)
	}
	c.Enabled = enabled == 1
	if c.APIKey, err = cryptoutil.Decrypt(enc, s.secret); err != nil {
		return nil, err
	}
	return &c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
