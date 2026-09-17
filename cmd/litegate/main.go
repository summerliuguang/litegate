// LiteGate：轻量级 AI 网关。单二进制 + SQLite，数据面与管理面同端口。
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"litegate/internal/api"
	"litegate/internal/config"
	"litegate/internal/store"
	"litegate/web"
)

func main() {
	addr := flag.String("addr", ":8080", "监听地址，如 :8080 或 127.0.0.1:8080")
	dbPath := flag.String("db", "litegate.db", "SQLite 数据库文件路径")
	showVersion := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(api.Version)
		return
	}

	cfg, err := config.Load(*addr, *dbPath)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	var secret []byte
	if cfg.Secret != "" {
		b, err := hex.DecodeString(cfg.Secret)
		// 配了但不合法是运维失误：静默回退会换一把主密钥导致旧密文全部解不开，
		// 必须拒绝启动把问题暴露在部署时。未配置则由 store 自动生成并落库。
		if err != nil || len(b) != 32 {
			log.Fatal("启动失败：LITEGATE_SECRET 已设置但不是 64 位十六进制（32 字节），拒绝以错误的主密钥启动")
		}
		secret = b
	}

	st, err := store.Open(cfg.DBPath, secret)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// 一次性迁移：空账本 + 有历史日志时，把日志聚合进 key_usage_day
	// （币种按当前价格表判定）。之后预算回填只依赖账本，日志清理不影响预算。
	if n, err := st.BackfillKeyUsageFromLogs(); err != nil {
		log.Printf("key_usage_day 回填失败（不影响启动）: %v", err)
	} else if n > 0 {
		log.Printf("已从请求日志回填密钥按日用量 %d 组（key_usage_day）", n)
	}

	if n, err := st.CountAPIKeys(); err == nil && n == 0 {
		k := &store.APIKey{Name: "default"}
		if err := st.CreateAPIKey(k); err == nil {
			log.Printf("已生成默认虚拟密钥 %s （仅此一次显示，请立即记入 deploy/litegate.env 的 LITEGATE_API_KEY；"+
				"下游客户端用它在网关鉴权，日志不会再次输出）", k.Key)
		}
	}

	handler := api.NewServer(st, cfg.AdminPassword, web.Handler(), cfg.PanelHosts)
	api.StartKeyHealthChecker(st, 2*time.Minute)

	// 日志保留策略：启动时清一次，之后每 6 小时清一次；0 天（默认）= 永久保留。
	// key_usage_day 账本独立保留 400 天（覆盖最长月预算窗口 + 余量）。
	if n, err := st.PruneKeyUsageDays(400); err == nil && n > 0 {
		log.Printf("已清理 %d 行过期的密钥按日用量", n)
	}
	if cfg.LogRetentionDays > 0 {
		go func(days int) {
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for {
				if n, err := st.PruneLogs(days); err != nil {
					log.Printf("清理 %d 天前日志失败: %v", days, err)
				} else if n > 0 {
					log.Printf("已清理 %d 天前的请求日志 %d 条", days, n)
				}
				if n, err := st.PruneKeyUsageDays(400); err != nil {
					log.Printf("清理密钥按日用量失败: %v", err)
				} else if n > 0 {
					log.Printf("已清理 %d 行过期的密钥按日用量", n)
				}
				<-ticker.C
			}
		}(cfg.LogRetentionDays)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅退出窗口：默认 30s（原 5s 会掐断所有超过 5 秒的在途流式请求），
	// LITEGATE_SHUTDOWN_TIMEOUT 秒数可调；systemd 侧 TimeoutStopSec 需大于该值。
	shutdownGrace := 30 * time.Second
	if s := os.Getenv("LITEGATE_SHUTDOWN_TIMEOUT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			shutdownGrace = time.Duration(n) * time.Second
		}
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	display := cfg.Addr
	if strings.HasPrefix(display, ":") {
		display = "localhost" + display
	}
	log.Printf("LiteGate %s 已启动：http://%s （管理页面与管理 API 同端口，数据库 %s）", api.Version, display, cfg.DBPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
