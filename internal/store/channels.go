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
	Models    []string `json:"models"`         // 启用的模型；为空表示全部（通配）
	DisabledModels []string `json:"disabled_models"` // 已禁用的模型，等待重新启用
	Weight    int      `json:"weight"`
	Priority  int      `json:"priority"`
	Enabled   bool     `json:"enabled"`
	Remark    string   `json:"remark"`
	CreatedAt string   `json:"created_at"`
}

// normalizeModels 保证不变量：models 与 disabled_models 不相交。
func normalizeModels(models, disabled []string) ([]string, []string) {
	dis := make([]string, 0, len(disabled))
	seen := map[string]bool{}
	for _, m := range disabled {
		if m != "" && !seen[m] {
			seen[m] = true
			dis = append(dis, m)
		}
	}
	out := models[:0:0]
	for _, m := range models {
		if m == "" || seen[m] {
			continue
		}
		out = append(out, m)
	}
	return out, dis
}

func (s *Store) CreateChannel(c *Channel) (int64, error) {
	if c.Weight <= 0 {
		c.Weight = 1
	}
	c.Models, c.DisabledModels = normalizeModels(c.Models, c.DisabledModels)
	models, err := json.Marshal(c.Models)
	if err != nil {
		return 0, err
	}
	disabled, err := json.Marshal(c.DisabledModels)
	if err != nil {
		return 0, err
	}
	enc, err := cryptoutil.Encrypt(c.APIKey, s.secret)
	if err != nil {
		return 0, err
	}
	res, err := s.DB.Exec(
		`INSERT INTO channels(name, type, base_url, api_key, models, disabled_models, weight, priority, enabled, remark)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"), enc,
		string(models), string(disabled), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark,
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
	c.Models, c.DisabledModels = normalizeModels(c.Models, c.DisabledModels)
	models, err := json.Marshal(c.Models)
	if err != nil {
		return err
	}
	disabled, err := json.Marshal(c.DisabledModels)
	if err != nil {
		return err
	}
	enc, err := cryptoutil.Encrypt(c.APIKey, s.secret)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(
		`UPDATE channels SET name=?, type=?, base_url=?, api_key=?, models=?, disabled_models=?,
		 weight=?, priority=?, enabled=?, remark=? WHERE id=?`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"), enc,
		string(models), string(disabled), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark, c.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableModel 禁用单个模型：从启用列表移除并记入禁用列表（通配渠道只记禁用列表）。
func (s *Store) DisableModel(id int64, model string) error {
	c, err := s.GetChannel(id)
	if err != nil {
		return err
	}
	models := c.Models[:0:0]
	for _, m := range c.Models {
		if m != model {
			models = append(models, m)
		}
	}
	disabled := c.DisabledModels
	if !containsModel(disabled, model) {
		disabled = append(append([]string{}, disabled...), model)
	}
	c.Models, c.DisabledModels = normalizeModels(models, disabled)
	return s.UpdateChannel(c)
}

// EnableModel 重新启用单个模型：从禁用列表移除并加回启用列表。
// 通配渠道（models 为空）只移除禁用项，不追加 models——否则会静默退化成单模型渠道。
func (s *Store) EnableModel(id int64, model string) error {
	c, err := s.GetChannel(id)
	if err != nil {
		return err
	}
	disabled := c.DisabledModels[:0:0]
	for _, m := range c.DisabledModels {
		if m != model {
			disabled = append(disabled, m)
		}
	}
	var models []string
	if len(c.Models) > 0 {
		models = c.Models
		if !containsModel(models, model) {
			models = append(append([]string{}, c.Models...), model)
		}
	}
	c.Models, c.DisabledModels = normalizeModels(models, disabled)
	return s.UpdateChannel(c)
}

func containsModel(list []string, model string) bool {
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}

// ServesModel 报告渠道当前是否服务该模型：显式列表里必须存在；通配渠道（models 为空）
// 服务任意模型，但禁用列表始终生效。
func (c *Channel) ServesModel(model string) bool {
	if containsModel(c.DisabledModels, model) {
		return false
	}
	if len(c.Models) == 0 {
		return true
	}
	return containsModel(c.Models, model)
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
		`SELECT id, name, type, base_url, api_key, models, disabled_models, weight, priority, enabled, remark, created_at
		 FROM channels WHERE id = ?`, id,
	))
}

// ListChannels 按 type 过滤（空串表示全部），优先级高的在前。
func (s *Store) ListChannels(typ string) ([]Channel, error) {
	q := `SELECT id, name, type, base_url, api_key, models, disabled_models, weight, priority, enabled, remark, created_at FROM channels`
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
	var models, disabled, enc string
	var enabled int
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &enc, &models, &disabled,
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
	if disabled != "" {
		_ = json.Unmarshal([]byte(disabled), &c.DisabledModels)
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
