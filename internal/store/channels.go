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
	APIKeys   []ChannelKey `json:"-"` // 解密后的明文，仅供代理转发使用，禁止序列化
	Models    []string `json:"models"`          // 启用的模型；为空表示全部（通配）
	DisabledModels []string `json:"disabled_models"` // 已禁用的模型，等待重新启用
	ModelMap  map[string]string `json:"model_map"` // 模型映射：对外名 → 上游真实名
	Weight    int      `json:"weight"`
	Priority  int      `json:"priority"`
	Enabled   bool     `json:"enabled"`
	Remark    string   `json:"remark"`
	CreatedAt string   `json:"created_at"`
}

// ChannelKey 是渠道的一把上游凭证；Key 明文不出存储层与代理层。
type ChannelKey struct {
	ID      int64  `json:"id"`
	Key     string `json:"-"`
	Masked  string `json:"masked"`
	Enabled bool   `json:"enabled"`
}

// FirstKey 返回第一把启用密钥的明文（健康检查等单 key 场景用）。
func (c *Channel) FirstKey() string {
	for _, k := range c.APIKeys {
		if k.Enabled {
			return k.Key
		}
	}
	return ""
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
	mm, err := json.Marshal(c.ModelMap)
	if err != nil {
		return 0, err
	}
	res, err := s.DB.Exec(
		`INSERT INTO channels(name, type, base_url, models, disabled_models, model_map, weight, priority, enabled, remark)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"),
		string(models), string(disabled), string(mm), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := s.replaceChannelKeys(id, c.APIKeys); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateChannel 全量更新渠道基础字段。APIKeys 为 nil 表示密钥不动；
// 非 nil（含空切片）时全量替换，替换时保留仍存在的密钥的启停状态。
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
	mm, err := json.Marshal(c.ModelMap)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(
		`UPDATE channels SET name=?, type=?, base_url=?, models=?, disabled_models=?, model_map=?,
		 weight=?, priority=?, enabled=?, remark=? WHERE id=?`,
		c.Name, c.Type, strings.TrimRight(c.BaseURL, "/"),
		string(models), string(disabled), string(mm), c.Weight, c.Priority, boolToInt(c.Enabled), c.Remark, c.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if c.APIKeys != nil {
		if err := s.replaceChannelKeys(c.ID, c.APIKeys); err != nil {
			return err
		}
	}
	return nil
}

// replaceChannelKeys 全量替换渠道密钥。按解密后的明文比对旧密钥，
// 保留仍存在密钥的手动启停状态（渠道普通更新不会重置禁用的密钥）。
// AES-GCM 带随机 nonce，密文不可比对，只能以明文为键。
func (s *Store) replaceChannelKeys(channelID int64, keys []ChannelKey) error {
	old, err := s.channelKeyStates(channelID)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`DELETE FROM channel_keys WHERE channel_id = ?`, channelID); err != nil {
		return err
	}
	for i, k := range keys {
		enc, err := cryptoutil.Encrypt(k.Key, s.secret)
		if err != nil {
			return err
		}
		enabled := true
		if prev, ok := old[k.Key]; ok {
			enabled = prev
		}
		if _, err := s.DB.Exec(
			`INSERT INTO channel_keys(channel_id, key_enc, enabled, sort) VALUES(?, ?, ?, ?)`,
			channelID, enc, boolToInt(enabled), i); err != nil {
			return err
		}
	}
	return nil
}

// channelKeyStates 返回现有密钥的 明文 → 启停 映射。
func (s *Store) channelKeyStates(channelID int64) (map[string]bool, error) {
	rows, err := s.DB.Query(
		`SELECT key_enc, enabled FROM channel_keys WHERE channel_id = ? ORDER BY sort, id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]bool{}
	for rows.Next() {
		var enc string
		var enabled int
		if err := rows.Scan(&enc, &enabled); err != nil {
			return nil, err
		}
		if plain, err := cryptoutil.Decrypt(enc, s.secret); err == nil {
			states[plain] = enabled == 1
		}
	}
	return states, rows.Err()
}

// SetChannelKeyEnabled 手动启停渠道上的单把密钥。
func (s *Store) SetChannelKeyEnabled(keyID int64, enabled bool) error {
	res, err := s.DB.Exec(`UPDATE channel_keys SET enabled = ? WHERE id = ?`, boolToInt(enabled), keyID)
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

// UpstreamModel 返回映射后的上游真实模型名；无映射时原样返回。
func (c *Channel) UpstreamModel(model string) string {
	if c.ModelMap == nil {
		return model
	}
	if real, ok := c.ModelMap[model]; ok && real != "" {
		return real
	}
	return model
}

func (s *Store) DeleteChannel(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := s.DB.Exec(`DELETE FROM channel_keys WHERE channel_id = ?`, id); err != nil {
		return err
	}
	return nil
}

func (s *Store) GetChannel(id int64) (*Channel, error) {
	c, err := s.scanChannel(s.DB.QueryRow(
		`SELECT id, name, type, base_url, models, disabled_models, model_map, weight, priority, enabled, remark, created_at
		 FROM channels WHERE id = ?`, id,
	))
	if err != nil {
		return nil, err
	}
	keys, err := s.loadChannelKeys([]int64{c.ID})
	if err != nil {
		return nil, err
	}
	c.APIKeys = keys[c.ID]
	return c, nil
}

// GetChannelByName 按名称取渠道（配置导入按名字 upsert 用）；不存在返回 ErrNotFound。
func (s *Store) GetChannelByName(name string) (*Channel, error) {
	c, err := s.scanChannel(s.DB.QueryRow(
		`SELECT id, name, type, base_url, models, disabled_models, model_map, weight, priority, enabled, remark, created_at
		 FROM channels WHERE name = ?`, name,
	))
	if err != nil {
		return nil, err
	}
	keys, err := s.loadChannelKeys([]int64{c.ID})
	if err != nil {
		return nil, err
	}
	c.APIKeys = keys[c.ID]
	return c, nil
}

// ListChannels 按 type 过滤（空串表示全部），优先级高的在前。
func (s *Store) ListChannels(typ string) ([]Channel, error) {
	q := `SELECT id, name, type, base_url, models, disabled_models, model_map, weight, priority, enabled, remark, created_at FROM channels`
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(out))
	for i := range out {
		ids[i] = out[i].ID
	}
	keys, err := s.loadChannelKeys(ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].APIKeys = keys[out[i].ID]
	}
	return out, nil
}

// loadChannelKeys 一次取回多个渠道的密钥并解密分组；被手动禁用的密钥带 Enabled=false 返回。
func (s *Store) loadChannelKeys(channelIDs []int64) (map[int64][]ChannelKey, error) {
	out := map[int64][]ChannelKey{}
	if len(channelIDs) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(
		`SELECT id, channel_id, key_enc, enabled FROM channel_keys
		 WHERE channel_id IN (` + intsPlaceholder(len(channelIDs)) + `) ORDER BY channel_id, sort, id`,
		intsToAny(channelIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k ChannelKey
		var cid int64
		var enc string
		var enabled int
		if err := rows.Scan(&k.ID, &cid, &enc, &enabled); err != nil {
			return nil, err
		}
		plain, err := cryptoutil.Decrypt(enc, s.secret)
		if err != nil {
			return nil, err
		}
		k.Key = plain
		k.Masked = MaskKey(plain)
		k.Enabled = enabled == 1
		out[cid] = append(out[cid], k)
	}
	return out, rows.Err()
}

func intsPlaceholder(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func intsToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// MaskKey 打码密钥：保留前 4 与末 4 位，中间以 **** 代替（管理面展示用）。
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + "****" + k[len(k)-4:]
}

type scanner interface{ Scan(dest ...any) error }

func (s *Store) scanChannel(row scanner) (*Channel, error) {
	var c Channel
	var models, disabled, mm string
	var enabled int
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &models, &disabled, &mm,
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
	if mm != "" {
		_ = json.Unmarshal([]byte(mm), &c.ModelMap)
	}
	c.Enabled = enabled == 1
	return &c, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
