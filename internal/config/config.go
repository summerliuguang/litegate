// Package config 汇集进程级配置：命令行参数 + 环境变量。
package config

import (
	"errors"
	"os"
)

type Config struct {
	Addr          string
	DBPath        string
	AdminPassword string
	// Secret 为可选的 32 字节十六进制主密钥，用于渠道凭证加密；
	// 未提供时自动生成并持久化在数据库 settings 表中。
	Secret string
	// LogRetentionDays 为请求日志保留天数；0 表示永久保留。
	LogRetentionDays int
}

// ErrWeakPassword 在未设置管理密码（或仍是演示默认值）时返回，
// 调用方必须拒绝启动——管理面可取出全部上游凭证，弱密码等于裸奔。
var ErrWeakPassword = errors.New("LITEGATE_ADMIN_PASSWORD 未设置或为演示默认值 admin，拒绝启动")

func Load(addr, dbPath string) (Config, error) {
	pw := os.Getenv("LITEGATE_ADMIN_PASSWORD")
	if pw == "" || pw == "admin" {
		return Config{}, ErrWeakPassword
	}
	retention := atoiEnv("LITEGATE_LOG_RETENTION_DAYS")
	return Config{
		Addr:             addr,
		DBPath:           dbPath,
		AdminPassword:    pw,
		Secret:           os.Getenv("LITEGATE_SECRET"),
		LogRetentionDays: retention,
	}, nil
}

func atoiEnv(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
