// Package store 负责 SQLite 存取：渠道、虚拟密钥、请求日志与系统设置。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB     *sql.DB
	secret []byte
}

var ErrNotFound = errors.New("record not found")

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS channels (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	type       TEXT NOT NULL,
	base_url   TEXT NOT NULL,
	api_key    TEXT NOT NULL DEFAULT '',
	models     TEXT NOT NULL DEFAULT '[]',
	weight     INTEGER NOT NULL DEFAULT 1,
	priority   INTEGER NOT NULL DEFAULT 0,
	enabled    INTEGER NOT NULL DEFAULT 1,
	remark     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS api_keys (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	key        TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL DEFAULT '',
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS request_logs (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	ts                TEXT NOT NULL DEFAULT (datetime('now')),
	api_key_id        INTEGER NOT NULL DEFAULT 0,
	channel_id        INTEGER NOT NULL DEFAULT 0,
	model             TEXT NOT NULL DEFAULT '',
	protocol          TEXT NOT NULL DEFAULT '',
	status            INTEGER NOT NULL DEFAULT 0,
	latency_ms        INTEGER NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost              REAL NOT NULL DEFAULT 0,
	ttfb_ms           INTEGER NOT NULL DEFAULT 0,
	error             TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_channel ON request_logs(channel_id);
CREATE TABLE IF NOT EXISTS model_prices (
	model        TEXT PRIMARY KEY,
	input_price  REAL NOT NULL DEFAULT 0,
	output_price REAL NOT NULL DEFAULT 0,
	updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
`

// Open 打开（必要时创建）数据库。secret 为 32 字节 AES 主密钥；
// 为空时读取 settings 中已持久化的密钥，仍不存在则自动生成。
func Open(path string, secret []byte) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// 单连接串行化读写，规避 SQLite 锁竞争；日志批量落库的优化留到用量统计阶段。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	s := &Store{DB: db}
	key, err := s.loadOrCreateSecret(secret)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.secret = key
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// migrate 为 M1 时期创建的旧库补齐后加的列（SQLite 不支持 ADD COLUMN IF NOT EXISTS）。
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range []struct{ col, ddl string }{
		{"cost", `ALTER TABLE request_logs ADD COLUMN cost REAL NOT NULL DEFAULT 0`},
		{"ttfb_ms", `ALTER TABLE request_logs ADD COLUMN ttfb_ms INTEGER NOT NULL DEFAULT 0`},
	} {
		if !have[m.col] {
			if _, err := db.Exec(m.ddl); err != nil {
				return fmt.Errorf("add column %s: %w", m.col, err)
			}
		}
	}
	return nil
}

func (s *Store) Secret() []byte { return s.secret }

func (s *Store) loadOrCreateSecret(env []byte) ([]byte, error) {
	if len(env) == 32 {
		return env, nil
	}
	const name = "secret_key"
	var v string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key = ?`, name).Scan(&v)
	if err == nil {
		if b, derr := hex.DecodeString(v); derr == nil && len(b) == 32 {
			return b, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		name, hex.EncodeToString(b),
	)
	if err != nil {
		return nil, err
	}
	return b, nil
}
