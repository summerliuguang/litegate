package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"litegate/internal/cryptoutil"
)

// APIKey 是下游客户端访问网关用的虚拟密钥。
type APIKey struct {
	ID            int64    `json:"id"`
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"` // 允许调用的模型；为空表示不限制（可用全部启用模型）
	Enabled       bool     `json:"enabled"`
	CreatedAt     string   `json:"created_at"`
}

// AllowsModel 报告该密钥是否允许调用某模型（空列表 = 不限制）。
func (k *APIKey) AllowsModel(model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	return containsModel(k.AllowedModels, model)
}

func scanAllowedModels(s string) []string {
	var out []string
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// CreateAPIKey 生成并持久化一个新的 sk- 前缀虚拟密钥。
func (s *Store) CreateAPIKey(name string, allowedModels []string) (*APIKey, error) {
	allowed, err := json.Marshal(normalizeList(allowedModels))
	if err != nil {
		return nil, err
	}
	k := &APIKey{Key: "sk-lg-" + cryptoutil.RandomHex(16), Name: name, AllowedModels: normalizeList(allowedModels), Enabled: true}
	res, err := s.DB.Exec(`INSERT INTO api_keys(key, name, allowed_models, enabled) VALUES(?, ?, ?, 1)`,
		k.Key, k.Name, string(allowed))
	if err != nil {
		return nil, err
	}
	if k.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return k, nil
}

// UpdateAPIKey 更新密钥的名称与模型限制（全量替换语义）。
func (s *Store) UpdateAPIKey(id int64, name string, allowedModels []string) error {
	allowed, err := json.Marshal(normalizeList(allowedModels))
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(`UPDATE api_keys SET name = ?, allowed_models = ? WHERE id = ?`,
		name, string(allowed), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.DB.Query(`SELECT id, key, name, allowed_models, enabled, created_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		var allowed string
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &allowed, &enabled, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.AllowedModels = scanAllowedModels(allowed)
		k.Enabled = enabled == 1
		out = append(out, k)
	}
	return out, rows.Err()
}

// LookupAPIKey 仅匹配处于启用状态的密钥。
func (s *Store) LookupAPIKey(key string) (*APIKey, error) {
	var k APIKey
	var enabled int
	var allowed string
	err := s.DB.QueryRow(
		`SELECT id, key, name, allowed_models, enabled, created_at FROM api_keys WHERE key = ? AND enabled = 1`, key,
	).Scan(&k.ID, &k.Key, &k.Name, &allowed, &enabled, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.AllowedModels = scanAllowedModels(allowed)
	k.Enabled = enabled == 1
	return &k, nil
}

func (s *Store) GetAPIKey(id int64) (*APIKey, error) {
	var k APIKey
	var enabled int
	var allowed string
	err := s.DB.QueryRow(
		`SELECT id, key, name, allowed_models, enabled, created_at FROM api_keys WHERE id = ?`, id,
	).Scan(&k.ID, &k.Key, &k.Name, &allowed, &enabled, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.AllowedModels = scanAllowedModels(allowed)
	k.Enabled = enabled == 1
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

// normalizeList 去空与去重。
func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}
