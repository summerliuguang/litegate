// Package config 汇集进程级配置：命令行参数 + 环境变量。
package config

import (
	"errors"
	"os"
	"strings"
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
	// PanelHosts 是面板对外服务的 Host 白名单（host:port）。SSO 回跳地址只从
	// 白名单里选，防止伪造 Host 头构造任意回跳（开放重定向）。空 = api 包内置默认。
	PanelHosts []string
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
		PanelHosts:       parseListEnv("LITEGATE_PANEL_HOSTS"),
	}, nil
}

// parseListEnv 解析逗号分隔的环境变量；未设置或全空时返回 nil（调用方回退默认值）。
func parseListEnv(name string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(name), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
