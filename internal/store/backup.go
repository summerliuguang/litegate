package store

// SQLite 运维：在线备份（VACUUM INTO，不阻塞读、短暂锁写）与备份文件列表。
// 备份目录固定在数据库文件同级的 backups/ 下。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// BackupInfo 是一份备份文件的元数据。
type BackupInfo struct {
	File    string `json:"file"` // 文件名（不含目录）
	Size    int64  `json:"size"`
	Created string `json:"created"` // UTC "YYYY-MM-DD HH:MM:SS"
}

// BackupDir 返回备份目录（数据库同级 backups/）。
func (s *Store) BackupDir() string {
	return filepath.Join(filepath.Dir(s.path), "backups")
}

// Backup 在线备份当前数据库到 backups/，返回备份文件名与字节数。
// VACUUM INTO 生成的是紧凑后的完整一致快照；目标文件已存在时报错（文件名带秒级时间戳，正常不会撞）。
func (s *Store) Backup() (BackupInfo, error) {
	dir := s.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupInfo{}, err
	}
	name := "litegate-" + time.Now().UTC().Format("20060102-150405") + ".db"
	target := filepath.Join(dir, name)
	if _, err := s.DB.Exec(`VACUUM INTO ?`, target); err != nil {
		return BackupInfo{}, fmt.Errorf("vacuum into: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return BackupInfo{}, err
	}
	return BackupInfo{File: name, Size: info.Size(),
		Created: info.ModTime().UTC().Format("2006-01-02 15:04:05")}, nil
}

// ListBackups 列出备份目录里的 .db 文件，新的在前。
func (s *Store) ListBackups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(s.BackupDir())
	if os.IsNotExist(err) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".db" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{File: e.Name(), Size: info.Size(),
			Created: info.ModTime().UTC().Format("2006-01-02 15:04:05")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File > out[j].File })
	return out, nil
}
