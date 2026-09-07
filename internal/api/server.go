// Package api 组装 LiteGate 的管理面与数据面 HTTP 路由。
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"litegate/internal/store"
)

// Version 由 main 通过 ldflags 注入（-X litegate/internal/api.Version=vx.y.z）。
var Version = "dev"

var startedAt = time.Now()

// NewServer 组装全部路由。webHandler 为内嵌管理页（可为 nil，便于测试）。
func NewServer(st *store.Store, adminPassword string, webHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	a := &admin{
		st:       st,
		password: adminPassword,
		sessions: map[string]time.Time{},
		failures: map[string]int{},
		locked:   map[string]time.Time{},
	}
	a.register(mux)

	p := &proxy{
		st:     st,
		client: newUpstreamClient(),
		keys:   newKeyHealthManager(),
		limits: newKeyAdmission(),
	}
	p.limits.load(st)
	serverProxy = p
	p.register(mux)

	a.invalidateModels = p.invalidateModelsCache
	a.invalidatePrices = p.invalidatePriceCache

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deep") == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		// 深度检查：真实读一次库，附带版本与运行时长
		deep := map[string]any{
			"status":         "ok",
			"version":        Version,
			"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		}
		var one int
		if err := st.DB.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			deep["status"] = "fail"
			deep["db"] = err.Error()
			writeJSON(w, http.StatusServiceUnavailable, deep)
			return
		}
		deep["db"] = "ok"
		writeJSON(w, http.StatusOK, deep)
	})

	if webHandler != nil {
		mux.Handle("/", webHandler)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// readJSON 解析请求体 JSON；超出限制或格式错误时直接写 400 并返回 error。
func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return err
	}
	return nil
}
