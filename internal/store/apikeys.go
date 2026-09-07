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
	// 治理字段：0/空 = 不限制。ExpiresAt 格式 YYYY-MM-DD（当日 23:59:59 本地失效）。
	ExpiresAt    string  `json:"expires_at"`
	RPMLimit     int64   `json:"rpm_limit"`
	TPMLimit     int64   `json:"tpm_limit"`
	BudgetUSD    float64 `json:"budget_usd"`
	BudgetPeriod string  `json:"budget_period"` // daily | monthly
	BudgetTokens int64   `json:"budget_tokens"`
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

const apiKeyColumns = `id, key, name, allowed_models, enabled, created_at,
	expires_at, rpm_limit, tpm_limit, budget_usd, budget_period, budget_tokens`

func scanAPIKey(scan func(dest ...any) error) (*APIKey, error) {
	var k APIKey
	var enabled int
	var allowed string
	if err := scan(&k.ID, &k.Key, &k.Name, &allowed, &enabled, &k.CreatedAt,
		&k.ExpiresAt, &k.RPMLimit, &k.TPMLimit, &k.BudgetUSD, &k.BudgetPeriod, &k.BudgetTokens); err != nil {
		return nil, err
	}
	k.AllowedModels = scanAllowedModels(allowed)
	k.Enabled = enabled == 1
	return &k, nil
}

// CreateAPIKey 生成并持久化一个新的 sk- 前缀虚拟密钥；治理字段取 k 中设置值。
func (s *Store) CreateAPIKey(k *APIKey) error {
	if k.Key == "" {
		k.Key = "sk-lg-" + cryptoutil.RandomHex(16)
	}
	k.AllowedModels = normalizeList(k.AllowedModels)
	if k.BudgetPeriod != "monthly" {
		k.BudgetPeriod = "daily"
	}
	allowed, err := json.Marshal(k.AllowedModels)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(
		`INSERT INTO api_keys(key, name, allowed_models, enabled, expires_at, rpm_limit, tpm_limit,
		     budget_usd, budget_period, budget_tokens)
		 VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
		k.Key, k.Name, string(allowed), k.ExpiresAt, k.RPMLimit, k.TPMLimit,
		k.BudgetUSD, k.BudgetPeriod, k.BudgetTokens)
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

// UpdateAPIKey 更新密钥的名称、模型限制与治理字段（全量替换语义）。
func (s *Store) UpdateAPIKey(k *APIKey) error {
	allowed, err := json.Marshal(normalizeList(k.AllowedModels))
	if err != nil {
		return err
	}
	if k.BudgetPeriod != "monthly" {
		k.BudgetPeriod = "daily"
	}
	res, err := s.DB.Exec(
		`UPDATE api_keys SET name = ?, allowed_models = ?, expires_at = ?, rpm_limit = ?,
		     tpm_limit = ?, budget_usd = ?, budget_period = ?, budget_tokens = ? WHERE id = ?`,
		k.Name, string(allowed), k.ExpiresAt, k.RPMLimit, k.TPMLimit,
		k.BudgetUSD, k.BudgetPeriod, k.BudgetTokens, k.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.DB.Query(`SELECT ` + apiKeyColumns + ` FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// LookupAPIKey 仅匹配处于启用状态的密钥。
func (s *Store) LookupAPIKey(key string) (*APIKey, error) {
	k, err := scanAPIKey(s.DB.QueryRow(
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key = ? AND enabled = 1`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

func (s *Store) GetAPIKey(id int64) (*APIKey, error) {
	k, err := scanAPIKey(s.DB.QueryRow(
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, nil
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

// UsageSince 汇总某密钥自 since（UTC "YYYY-MM-DD HH:MM:SS"）以来的成本与 token 总量，
// 供预算窗口使用；无记录返回 0。
func (s *Store) UsageSince(apiKeyID int64, since string) (cost float64, tokens int64, err error) {
	err = s.DB.QueryRow(
		`SELECT COALESCE(SUM(cost),0), COALESCE(SUM(prompt_tokens+completion_tokens),0)
		 FROM request_logs WHERE api_key_id = ? AND ts >= ?`, apiKeyID, since,
	).Scan(&cost, &tokens)
	return cost, tokens, err
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
