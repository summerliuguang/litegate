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
}

// ErrWeakPassword 在未设置管理密码（或仍是演示默认值）时返回，
// 调用方必须拒绝启动——管理面可取出全部上游凭证，弱密码等于裸奔。
var ErrWeakPassword = errors.New("LITEGATE_ADMIN_PASSWORD 未设置或为演示默认值 admin，拒绝启动")

func Load(addr, dbPath string) (Config, error) {
	pw := os.Getenv("LITEGATE_ADMIN_PASSWORD")
	if pw == "" || pw == "admin" {
		return Config{}, ErrWeakPassword
	}
	return Config{
		Addr:          addr,
		DBPath:        dbPath,
		AdminPassword: pw,
		Secret:        os.Getenv("LITEGATE_SECRET"),
	}, nil
}
