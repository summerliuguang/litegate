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
	// Auto 路由：客户端 model 传 "auto" 时按 AutoMode 从候选池解析真实模型。
	// AutoMode 空 = 未启用；AutoModels 空 = 候选池跟随 AllowedModels（两者都空 =
	// 全部启用模型）；AutoPriority 仅 priority 模式使用（未列出的候选追加在末尾）。
	AutoMode     string   `json:"auto_mode"` // latency | balance | priority | smart
	AutoModels   []string `json:"auto_models"`
	AutoPriority []string `json:"auto_priority"`
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
	expires_at, rpm_limit, tpm_limit, budget_usd, budget_period, budget_tokens,
	auto_mode, auto_models, auto_priority`

func scanAPIKey(scan func(dest ...any) error) (*APIKey, error) {
	var k APIKey
	var enabled int
	var allowed, autoModels, autoPriority string
	if err := scan(&k.ID, &k.Key, &k.Name, &allowed, &enabled, &k.CreatedAt,
		&k.ExpiresAt, &k.RPMLimit, &k.TPMLimit, &k.BudgetUSD, &k.BudgetPeriod, &k.BudgetTokens,
		&k.AutoMode, &autoModels, &autoPriority); err != nil {
		return nil, err
	}
	k.AllowedModels = scanAllowedModels(allowed)
	k.AutoModels = scanAllowedModels(autoModels)
	k.AutoPriority = scanAllowedModels(autoPriority)
	k.Enabled = enabled == 1
	return &k, nil
}

// CreateAPIKey 生成并持久化一个新的 sk- 前缀虚拟密钥；治理字段取 k 中设置值。
func (s *Store) CreateAPIKey(k *APIKey) error {
	if k.Key == "" {
		k.Key = "sk-lg-" + cryptoutil.RandomHex(16)
	}
	k.AllowedModels = normalizeList(k.AllowedModels)
	k.AutoModels = normalizeList(k.AutoModels)
	k.AutoPriority = normalizeList(k.AutoPriority)
	if k.BudgetPeriod != "monthly" {
		k.BudgetPeriod = "daily"
	}
	allowed, err := json.Marshal(k.AllowedModels)
	if err != nil {
		return err
	}
	autoModels, err := json.Marshal(k.AutoModels)
	if err != nil {
		return err
	}
	autoPriority, err := json.Marshal(k.AutoPriority)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(
		`INSERT INTO api_keys(key, name, allowed_models, enabled, expires_at, rpm_limit, tpm_limit,
		     budget_usd, budget_period, budget_tokens, auto_mode, auto_models, auto_priority)
		 VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Key, k.Name, string(allowed), k.ExpiresAt, k.RPMLimit, k.TPMLimit,
		k.BudgetUSD, k.BudgetPeriod, k.BudgetTokens, k.AutoMode, string(autoModels), string(autoPriority))
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

// UpdateAPIKey 更新密钥的名称、模型限制、治理字段与 auto 路由配置（全量替换语义）。
func (s *Store) UpdateAPIKey(k *APIKey) error {
	allowed, err := json.Marshal(normalizeList(k.AllowedModels))
	if err != nil {
		return err
	}
	autoModels, err := json.Marshal(normalizeList(k.AutoModels))
	if err != nil {
		return err
	}
	autoPriority, err := json.Marshal(normalizeList(k.AutoPriority))
	if err != nil {
		return err
	}
	if k.BudgetPeriod != "monthly" {
		k.BudgetPeriod = "daily"
	}
	res, err := s.DB.Exec(
		`UPDATE api_keys SET name = ?, allowed_models = ?, expires_at = ?, rpm_limit = ?,
		     tpm_limit = ?, budget_usd = ?, budget_period = ?, budget_tokens = ?,
		     auto_mode = ?, auto_models = ?, auto_priority = ? WHERE id = ?`,
		k.Name, string(allowed), k.ExpiresAt, k.RPMLimit, k.TPMLimit,
		k.BudgetUSD, k.BudgetPeriod, k.BudgetTokens,
		k.AutoMode, string(autoModels), string(autoPriority), k.ID)
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

// KeySpeedStat 是单个虚拟密钥的输出速度聚合。
type KeySpeedStat struct {
	APIKeyID         int64
	Requests         int64
	CompletionTokens int64
	GenMs            int64 // 生成阶段耗时合计（latency_ms - ttfb_ms）
}

// Tps 返回加权平均输出速度（输出 token/秒）；无有效样本返回 0。
func (st KeySpeedStat) Tps() float64 {
	if st.GenMs <= 0 {
		return 0
	}
	return float64(st.CompletionTokens) / (float64(st.GenMs) / 1000)
}

// KeySpeedStats 按密钥聚合近 7 天的输出速度：只统计成功且可计算的请求
// （有输出 token 且 latency > ttfb）。加权平均比逐条平均更能反映真实吞吐。
func (s *Store) KeySpeedStats() (map[int64]KeySpeedStat, error) {
	rows, err := s.DB.Query(`
		SELECT api_key_id, COUNT(*), SUM(completion_tokens), SUM(latency_ms - ttfb_ms)
		FROM request_logs
		WHERE ` + sqlLast7Days + ` AND status < 400 AND error = ''
		  AND completion_tokens > 0 AND latency_ms > ttfb_ms
		GROUP BY api_key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]KeySpeedStat{}
	for rows.Next() {
		var st KeySpeedStat
		if err := rows.Scan(&st.APIKeyID, &st.Requests, &st.CompletionTokens, &st.GenMs); err != nil {
			return nil, err
		}
		out[st.APIKeyID] = st
	}
	return out, rows.Err()
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

// ModelLatencyStats 按模型聚合近 24 小时成功请求的平均总延迟（毫秒），
// 供虚拟密钥 auto 路由的低延迟/智能模式选模型；无样本的模型不在结果中。
func (s *Store) ModelLatencyStats() (map[string]int64, error) {
	rows, err := s.DB.Query(`
		SELECT model, AVG(latency_ms)
		FROM request_logs
		WHERE ts >= datetime('now', '-1 day') AND status < 400 AND error = '' AND model != ''
		GROUP BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var model string
		var avg float64
		if err := rows.Scan(&model, &avg); err != nil {
			return nil, err
		}
		out[model] = int64(avg + 0.5)
	}
	return out, rows.Err()
}
