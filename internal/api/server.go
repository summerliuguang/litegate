// Package api 组装 LiteGate 的管理面与数据面 HTTP 路由。
package api

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"litegate/internal/store"
)

// Version 由 main 通过 ldflags 注入（-X litegate/internal/api.Version=vx.y.z）。
var Version = "dev"

var startedAt = time.Now()

// NewServer 组装全部路由。webHandler 为内嵌管理页（可为 nil，便于测试）。
// panelHosts 是 SSO 回跳允许的 Host 白名单（host:port）；空则用内置默认。
func NewServer(st *store.Store, adminPassword string, webHandler http.Handler, panelHosts []string) http.Handler {
	mux := http.NewServeMux()

	a := &admin{
		st:         st,
		password:   adminPassword,
		panelHosts: normalizePanelHosts(panelHosts),
		sessions:   map[string]adminSession{},
		failures:   map[string]int{},
		locked:     map[string]time.Time{},
	}
	alerts := newAlertManager(st)
	a.alerts = alerts
	a.register(mux)

	p := &proxy{
		st:          st,
		client:      newUpstreamClient(),
		idleTimeout: upstreamIdleTimeout(),
		keys:        newKeyHealthManager(st, alerts),
		limits:      newKeyAdmission(alerts),
		alerts:      alerts,
		metrics:     newMetricsState(),
	}
	p.keys.restore(st) // 恢复重启前未到期的密钥冷却，避免对坏上游惊群重试
	p.limits.load(st)
	serverProxy = p
	p.register(mux)

	a.invalidateModels = p.invalidateModelsCache
	a.invalidatePrices = p.invalidatePriceCache
	a.invalidateBodyLog = p.invalidateBodyLogCfg
	a.budgetUsage = p.limits.budgetUsage
	a.invalidateRate = p.limits.invalidateRate

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deep") == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		// 深度检查：库可读 + 可写 + 主密钥加解密自检，附带版本与运行时长
		deep := map[string]any{
			"status":         "ok",
			"version":        Version,
			"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		}
		fail := func(item, msg string) {
			deep["status"] = "fail"
			deep[item] = msg
			writeJSON(w, http.StatusServiceUnavailable, deep)
		}
		var one int
		if err := st.DB.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			fail("db_read", err.Error())
			return
		}
		deep["db_read"] = "ok"
		if err := st.HealthCheck(); err != nil {
			fail("db_write", err.Error())
			return
		}
		deep["db_write"] = "ok"
		deep["crypto"] = "ok"
		writeJSON(w, http.StatusOK, deep)
	})

	// /metrics：本机直连放行（Prometheus/脚本抓取）；经反代访问要求管理令牌
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		viaProxy := r.Header.Get("X-Real-IP") != "" || r.Header.Get("X-Forwarded-For") != ""
		if viaProxy || !isLoopbackRemote(r.RemoteAddr) {
			if !a.hasSession(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "admin token required"})
				return
			}
		}
		p.serveMetrics(w, r)
	})

	if webHandler != nil {
		mux.Handle("/", webHandler)
	}
	return securityHeaders(mux)
}

// securityHeaders 给所有响应补基础安全头。管理页是浏览器唯一入口，CSP 收紧
// 外链与内嵌（内联 script/style 是免构建内嵌页的既有形态，保留 unsafe-inline）；
// API 响应多一层 nosniff / DENY 也无副作用。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// defaultPanelHosts 是未配置 LITEGATE_PANEL_HOSTS 时的回跳白名单：
// 局域网 nginx 入口 + 本机直连两种形态。
var defaultPanelHosts = []string{"192.168.5.15:29007", "127.0.0.1:8080", "localhost:8080"}

// normalizePanelHosts 归一化 Host 白名单：空白/未配置时回退默认。
func normalizePanelHosts(in []string) []string {
	var out []string
	for _, h := range in {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return defaultPanelHosts
	}
	return out
}

// isLoopbackRemote 判断来源地址是否为本机回环。
func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeInternalError 数据面内部错误出口：详情进服务端日志，客户端只给通用
// 文案——错误串可能带上游主机、内网 IP 或库文件路径，虚拟密钥持有者不应借此
// 探测内部拓扑。管理面错误仍直出 err.Error()，便于运维定位。
func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: internal error: %v", r.Method, r.URL.Path, err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal gateway error")
}

// writeErr 输出带机器可读错误码的 API 错误响应：客户端按 code 分支（如区分
// rpm 限速与预算耗尽），error 文案仅供人读、可随时调整。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// readJSON 解析请求体 JSON；超出限制或格式错误时直接写 400 并返回 error。
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return readJSONLimit(w, r, dst, 1<<20)
}

// readJSONLimit 同 readJSON,请求体上限由调用方给定(对话测试带附件时放宽到数据面上限)。
func readJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return err
	}
	return nil
}
