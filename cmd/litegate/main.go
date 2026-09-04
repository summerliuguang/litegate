// LiteGate：轻量级 AI 网关。单二进制 + SQLite，数据面与管理面同端口。
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	flag.Parse()

	cfg := config.Load(*addr, *dbPath)
	if cfg.AdminPassword == "admin" && os.Getenv("LITEGATE_ADMIN_PASSWORD") == "" {
		log.Print("警告：未设置 LITEGATE_ADMIN_PASSWORD，管理密码使用默认值 admin，请尽快修改")
	}

	var secret []byte
	if cfg.Secret != "" {
		if b, err := hex.DecodeString(cfg.Secret); err == nil && len(b) == 32 {
			secret = b
		} else {
			log.Print("警告：LITEGATE_SECRET 不是 64 位十六进制，已忽略，改用数据库内生成的主密钥")
		}
	}

	st, err := store.Open(cfg.DBPath, secret)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if n, err := st.CountAPIKeys(); err == nil && n == 0 {
		if k, err := st.CreateAPIKey("default"); err == nil {
			log.Printf("已生成默认虚拟密钥 %s （下游客户端用它在网关鉴权）", k.Key)
		}
	}

	handler := api.NewServer(st, cfg.AdminPassword, web.Handler())
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	display := cfg.Addr
	if strings.HasPrefix(display, ":") {
		display = "localhost" + display
	}
	log.Printf("LiteGate 已启动：http://%s （管理页面与管理 API 同端口，数据库 %s）", display, cfg.DBPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
