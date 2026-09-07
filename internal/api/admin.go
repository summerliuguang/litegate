package api

import (
	"crypto/subtle"
	"net"
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
	// invalidateModels/invalidatePrices 通知数据面失效对应缓存
	// （渠道或模型变更、价格变更时调用，漏调会导致最长 60 秒脏数据）。
	invalidateModels func()
	invalidatePrices func()

	mu       sync.Mutex
	sessions map[string]time.Time
	failures map[string]int     // 来源 IP → 连续登录失败次数
	locked   map[string]time.Time // 来源 IP → 锁定截止时间
}

func (a *admin) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/login", a.login)
	mux.Handle("GET /api/admin/dashboard", a.auth(a.dashboard))
	mux.Handle("GET /api/admin/channels", a.auth(a.listChannels))
	mux.Handle("POST /api/admin/channels", a.auth(a.createChannel))
	mux.Handle("PUT /api/admin/channels/{id}", a.auth(a.updateChannel))
	mux.Handle("DELETE /api/admin/channels/{id}", a.auth(a.deleteChannel))
	mux.Handle("POST /api/admin/channels/{id}/test", a.auth(a.testChannel))
	mux.Handle("POST /api/admin/channels/{id}/models/disable/{model...}", a.auth(a.disableChannelModel))
	mux.Handle("POST /api/admin/channels/{id}/models/enable/{model...}", a.auth(a.enableChannelModel))
	mux.Handle("GET /api/admin/keys", a.auth(a.listKeys))
	mux.Handle("POST /api/admin/keys", a.auth(a.createKey))
	mux.Handle("PUT /api/admin/keys/{id}", a.auth(a.updateKey))
	mux.Handle("GET /api/admin/keys/{id}/reveal", a.auth(a.revealKey))
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
	// 登录限速：同一来源 IP 连续失败 5 次锁定 60 秒（内存态，重启即清零）
	ip := clientIP(r)
	a.mu.Lock()
	if a.locked != nil {
		if until, ok := a.locked[ip]; ok {
			if time.Now().Before(until) {
				a.mu.Unlock()
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts, try later"})
				return
			}
			delete(a.locked, ip)
		}
	}
	a.mu.Unlock()

	var req struct {
		Password string `json:"password"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password)) != 1 {
		a.mu.Lock()
		if a.locked == nil {
			a.locked = map[string]time.Time{}
		}
		a.failures[ip]++
		if a.failures[ip] >= 5 {
			a.locked[ip] = time.Now().Add(60 * time.Second)
			delete(a.failures, ip)
		}
		a.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	tok := cryptoutil.RandomHex(32)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(7 * 24 * time.Hour)
	delete(a.failures, ip)
	for k, exp := range a.sessions { // 顺手清理过期会话
		if time.Now().After(exp) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// clientIP 提取来源 IP（家庭内网直接 RemoteAddr；经 nginx 时 X-Real-IP 可信）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
	// DisabledModels 可选：显式指定禁用列表（批量导入场景全量替换）。
	// 不传（nil）时按"移出启用列表即禁用"的规则自动推导。
	DisabledModels *[]string `json:"disabled_models"`
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
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	BaseURL        string   `json:"base_url"`
	APIKey         string   `json:"api_key"`
	Models         []string `json:"models"`
	DisabledModels []string `json:"disabled_models"`
	Weight         int      `json:"weight"`
	Priority       int      `json:"priority"`
	Enabled        bool     `json:"enabled"`
	Remark         string   `json:"remark"`
	CreatedAt      string   `json:"created_at"`
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
		APIKey: maskKey(c.APIKey), Models: c.Models, DisabledModels: c.DisabledModels,
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
	var disabled []string
	if in.DisabledModels != nil {
		disabled = *in.DisabledModels
	}
	id, err := a.st.CreateChannel(&store.Channel{
		Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Models: in.Models, DisabledModels: disabled, Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.invalidateModels()
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
	// 模型列表全量替换：移出启用列表的模型自动转入禁用列表（重新添加即恢复），
	// 这样不会丢失"曾经配置过"的信息，也符合"禁用后重新添加即启用"的操作习惯。
	// 调用方显式传入 disabled_models 时以其为准（批量导入场景）。
	var newDisabled []string
	if in.DisabledModels != nil {
		newDisabled = *in.DisabledModels
	} else {
		disabled := old.DisabledModels
		for _, m := range old.Models {
			if !containsStr(in.Models, m) && !containsStr(disabled, m) {
				disabled = append(disabled, m)
			}
		}
		newDisabled = disabled[:0:0]
		for _, m := range disabled {
			if !containsStr(in.Models, m) {
				newDisabled = append(newDisabled, m)
			}
		}
	}
	err = a.st.UpdateChannel(&store.Channel{
		ID: id, Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Models: in.Models, DisabledModels: newDisabled, Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark, CreatedAt: old.CreatedAt,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// containsStr 是 containsModel 的 api 包本地别名（store 里的同名助手未导出）。
func containsStr(list []string, s string) bool {
	for _, m := range list {
		if m == s {
			return true
		}
	}
	return false
}

// disableChannelModel 禁用渠道上的单个模型：POST .../models/disable/{model...}
func (a *admin) disableChannelModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if err := a.st.DisableModel(id, model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// enableChannelModel 重新启用渠道上的单个模型：POST .../models/enable/{model...}
func (a *admin) enableChannelModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if err := a.st.EnableModel(id, model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.DeleteChannel(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
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

// maskApiKey 打码虚拟密钥：保留前缀与末 4 位，中间以 **** 代替。
func maskApiKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "****" + k[len(k)-4:]
}

type apiKeyOut struct {
	ID            int64    `json:"id"`
	Key           string   `json:"key"` // 打码后的密钥，明文需经 reveal 端点按需获取
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	Enabled       bool     `json:"enabled"`
	CreatedAt     string   `json:"created_at"`
}

func (a *admin) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := a.st.ListAPIKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]apiKeyOut, 0, len(keys))
	for i := range keys {
		out = append(out, apiKeyOut{
			ID: keys[i].ID, Key: maskApiKey(keys[i].Key), Name: keys[i].Name,
			AllowedModels: keys[i].AllowedModels, Enabled: keys[i].Enabled, CreatedAt: keys[i].CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// revealKey 返回密钥明文。列表接口默认只给打码值，明文按需获取，避免管理页一屏明文。
func (a *admin) revealKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := a.st.GetAPIKey(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": k.Key})
}

func (a *admin) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	k, err := a.st.CreateAPIKey(req.Name, req.AllowedModels)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// updateKey 更新密钥的名称与模型限制（全量替换；allowed_models 留空 = 不限制）。
func (a *admin) updateKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := a.st.UpdateAPIKey(id, req.Name, req.AllowedModels); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if a.invalidatePrices != nil {
		a.invalidatePrices()
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *admin) deletePrice(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if err := a.st.DeleteModelPrice(model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "price not found"})
		return
	}
	if a.invalidatePrices != nil {
		a.invalidatePrices()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
