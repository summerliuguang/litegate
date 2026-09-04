// Package api 组装 LiteGate 的管理面与数据面 HTTP 路由。
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"litegate/internal/store"
)

// NewServer 组装全部路由。webHandler 为内嵌管理页（可为 nil，便于测试）。
func NewServer(st *store.Store, adminPassword string, webHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	a := &admin{
		st:       st,
		password: adminPassword,
		sessions: map[string]time.Time{},
	}
	a.register(mux)

	p := &proxy{
		st:     st,
		client: newUpstreamClient(),
	}
	p.register(mux)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
