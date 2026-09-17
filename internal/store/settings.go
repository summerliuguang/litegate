package store

// settings 表的通用读写：告警配置等运行时配置存这里（JSON 文本），管理面修改
// 即时生效，无需重启。

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"litegate/internal/cryptoutil"
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

// HealthCheck 深度健康检查的存储侧探测：真实写一遍 settings 并读回（验证库
// 可写、WAL 正常），再做一次加解密往返（验证主密钥可用——密钥失效时渠道凭证
// 全部取不出来，必须在健康检查里暴露）。返回首个失败项的错误；nil = 全部正常。
func (s *Store) HealthCheck() error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := s.SetSetting("health_ping", now); err != nil {
		return fmt.Errorf("db write: %w", err)
	}
	v, err := s.GetSetting("health_ping")
	if err != nil {
		return fmt.Errorf("db readback: %w", err)
	}
	if v != now {
		return errors.New("db readback mismatch")
	}
	enc, err := cryptoutil.Encrypt("health-check "+now, s.secret)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	plain, err := cryptoutil.Decrypt(enc, s.secret)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	if plain != "health-check "+now {
		return errors.New("crypto roundtrip mismatch")
	}
	return nil
}

// AdminAudit 是一条管理操作审计记录（ts 为 UTC "YYYY-MM-DD HH:MM:SS"）。
type AdminAudit struct {
	ID     int64  `json:"id"`
	Ts     string `json:"ts"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	IP     string `json:"ip"`
}

// auditKeepBounds 是审计保留条数的钳制范围：下限防误配为 0 关掉审计，
// 上限防无界表增长。
const (
	auditKeepMin = 100
	auditKeepMax = 100000
)

// AuditKeep 返回审计保留条数（settings 表 audit_keep，60s 缓存，默认 5000）。
// reveal/export 记账后审计写入频率上升，1000 条的旧上限会把渠道变更记录
// 挤掉，故默认提到 5000。
func (s *Store) AuditKeep() int {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditKeep > 0 && time.Now().Before(s.auditExpire) {
		return s.auditKeep
	}
	keep := 5000
	if v, err := s.GetSetting("audit_keep"); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil {
			keep = n
		}
	}
	if keep < auditKeepMin {
		keep = auditKeepMin
	}
	if keep > auditKeepMax {
		keep = auditKeepMax
	}
	s.auditKeep = keep
	s.auditExpire = time.Now().Add(time.Minute)
	return keep
}

// SetAuditKeep 写入保留条数并立即失效缓存。
func (s *Store) SetAuditKeep(n int) error {
	if n < auditKeepMin {
		n = auditKeepMin
	}
	if n > auditKeepMax {
		n = auditKeepMax
	}
	if err := s.SetSetting("audit_keep", strconv.Itoa(n)); err != nil {
		return err
	}
	s.auditMu.Lock()
	s.auditKeep = n
	s.auditExpire = time.Now().Add(time.Minute)
	s.auditMu.Unlock()
	return nil
}

// InsertAudit 记录一条管理操作；超过保留条数（audit_keep）时顺带清理最旧的
// （低频操作，开销可忽略）。
func (s *Store) InsertAudit(action, detail, ip string) error {
	_, err := s.DB.Exec(`INSERT INTO admin_audit(action, detail, ip) VALUES(?, ?, ?)`, action, detail, ip)
	if err != nil {
		return err
	}
	_, _ = s.DB.Exec(`DELETE FROM admin_audit WHERE id <= (SELECT MAX(id) - ? FROM admin_audit)`, s.AuditKeep())
	return nil
}

// ListAudit 返回最近的审计记录（新→旧）。limit 上限放到审计保留上限，
// 供全量导出使用。
func (s *Store) ListAudit(limit int) ([]AdminAudit, error) {
	if limit <= 0 || limit > auditKeepMax {
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
