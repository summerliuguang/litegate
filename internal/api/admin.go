package api

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"litegate/internal/cryptoutil"
	"litegate/internal/store"
)

type admin struct {
	st       *store.Store
	password string

	mu       sync.Mutex
	sessions map[string]time.Time
}

func (a *admin) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/login", a.login)
	mux.Handle("GET /api/admin/dashboard", a.auth(a.dashboard))
	mux.Handle("GET /api/admin/channels", a.auth(a.listChannels))
	mux.Handle("POST /api/admin/channels", a.auth(a.createChannel))
	mux.Handle("PUT /api/admin/channels/{id}", a.auth(a.updateChannel))
	mux.Handle("DELETE /api/admin/channels/{id}", a.auth(a.deleteChannel))
	mux.Handle("POST /api/admin/channels/{id}/test", a.auth(a.testChannel))
	mux.Handle("GET /api/admin/keys", a.auth(a.listKeys))
	mux.Handle("POST /api/admin/keys", a.auth(a.createKey))
	mux.Handle("DELETE /api/admin/keys/{id}", a.auth(a.deleteKey))
	mux.Handle("GET /api/admin/logs", a.auth(a.listLogs))
	mux.Handle("GET /api/admin/prices", a.auth(a.listPrices))
	mux.Handle("PUT /api/admin/prices", a.auth(a.upsertPrice))
	mux.Handle("DELETE /api/admin/prices/{model...}", a.auth(a.deletePrice))
}

func (a *admin) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a.mu.Lock()
		exp, ok := a.sessions[tok]
		a.mu.Unlock()
		if tok == "" || !ok || time.Now().After(exp) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	})
}

func (a *admin) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	tok := cryptoutil.RandomHex(32)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(7 * 24 * time.Hour)
	for k, exp := range a.sessions { // 顺手清理过期会话
		if time.Now().After(exp) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (a *admin) dashboard(w http.ResponseWriter, _ *http.Request) {
	d, err := a.st.Dashboard()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ---- 渠道管理 ----

type channelIn struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	Models   []string `json:"models"`
	Weight   int      `json:"weight"`
	Priority int      `json:"priority"`
	Enabled  *bool    `json:"enabled"`
	Remark   string   `json:"remark"`
}

func (in *channelIn) validate() string {
	if in.Name == "" {
		return "name is required"
	}
	if in.Type != "openai" && in.Type != "anthropic" {
		return "type must be openai or anthropic"
	}
	if in.BaseURL == "" {
		return "base_url is required"
	}
	return ""
}

// channelOut 是渠道的对外视图：api_key 打码，避免明文回显。
type channelOut struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BaseURL   string   `json:"base_url"`
	APIKey    string   `json:"api_key"`
	Models    []string `json:"models"`
	Weight    int      `json:"weight"`
	Priority  int      `json:"priority"`
	Enabled   bool     `json:"enabled"`
	Remark    string   `json:"remark"`
	CreatedAt string   `json:"created_at"`
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + "****" + k[len(k)-4:]
}

func maskChannel(c *store.Channel) channelOut {
	return channelOut{
		ID: c.ID, Name: c.Name, Type: c.Type, BaseURL: c.BaseURL,
		APIKey: maskKey(c.APIKey), Models: c.Models,
		Weight: c.Weight, Priority: c.Priority, Enabled: c.Enabled,
		Remark: c.Remark, CreatedAt: c.CreatedAt,
	}
}

func (a *admin) listChannels(w http.ResponseWriter, _ *http.Request) {
	chans, err := a.st.ListChannels("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]channelOut, 0, len(chans))
	for i := range chans {
		out = append(out, maskChannel(&chans[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *admin) createChannel(w http.ResponseWriter, r *http.Request) {
	var in channelIn
	if readJSON(w, r, &in) != nil {
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	id, err := a.st.CreateChannel(&store.Channel{
		Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Models: in.Models, Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (a *admin) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	old, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	var in channelIn
	if readJSON(w, r, &in) != nil {
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// api_key 留空表示沿用原凭证
	if in.APIKey == "" {
		in.APIKey = old.APIKey
	}
	enabled := old.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	err = a.st.UpdateChannel(&store.Channel{
		ID: id, Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Models: in.Models, Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark, CreatedAt: old.CreatedAt,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.DeleteChannel(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) testChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	ctx := r.Context()
	n, err := fetchUpstreamModels(ctx, upstreamClientForTest, c)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": n})
}

// ---- 虚拟密钥管理 ----

func (a *admin) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := a.st.ListAPIKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (a *admin) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	k, err := a.st.CreateAPIKey(req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (a *admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.DeleteAPIKey(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	f := store.LogFilter{
		Limit:  limit,
		Offset: offset,
		Model:  q.Get("model"),
		Status: q.Get("status"),
		Since:  q.Get("since"),
		Until:  q.Get("until"),
	}
	f.ChannelID, _ = strconv.ParseInt(q.Get("channel_id"), 10, 64)
	f.APIKeyID, _ = strconv.ParseInt(q.Get("api_key_id"), 10, 64)
	page, err := a.st.ListLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ---- 模型价格管理 ----

type priceIn struct {
	Model       string  `json:"model"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
}

func (a *admin) listPrices(w http.ResponseWriter, _ *http.Request) {
	prices, err := a.st.ListModelPrices()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, prices)
}

func (a *admin) upsertPrice(w http.ResponseWriter, r *http.Request) {
	var in priceIn
	if readJSON(w, r, &in) != nil {
		return
	}
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" || in.InputPrice < 0 || in.OutputPrice < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "model is required and prices must be non-negative"})
		return
	}
	p := &store.ModelPrice{Model: in.Model, InputPrice: in.InputPrice, OutputPrice: in.OutputPrice}
	if err := a.st.UpsertModelPrice(p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *admin) deletePrice(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if err := a.st.DeleteModelPrice(model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "price not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
