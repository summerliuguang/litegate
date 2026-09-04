// Package config 汇集进程级配置：命令行参数 + 环境变量。
package config

import "os"

type Config struct {
	Addr          string
	DBPath        string
	AdminPassword string
	// Secret 为可选的 32 字节十六进制主密钥，用于渠道凭证加密；
	// 未提供时自动生成并持久化在数据库 settings 表中。
	Secret string
}

func Load(addr, dbPath string) Config {
	pw := os.Getenv("LITEGATE_ADMIN_PASSWORD")
	if pw == "" {
		pw = "admin"
	}
	return Config{
		Addr:          addr,
		DBPath:        dbPath,
		AdminPassword: pw,
		Secret:        os.Getenv("LITEGATE_SECRET"),
	}
}
